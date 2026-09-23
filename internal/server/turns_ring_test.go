package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/sessions"
)

// The cross-node case this whole feature exists for: a caller that knows only
// a session id hits the LEADER with no ?node= and must still reach the
// follower that owns it — for submission, for the long-polled read, and for
// the 409/410 rejections, whose bodies have to survive the hop verbatim.

// fakeShell is a stand-in login shell: it answers a `-p --resume` command with
// a result object and treats anything else as the interactive session.
const fakeShell = `#!/bin/sh
cmd="$2"
case "$cmd" in
  *"-p --resume"*)
    prompt=$(cat)
    case "$prompt" in *SLOW*) sleep 3 ;; esac
    printf '{"type":"result","subtype":"success","is_error":false,"result":"echo: %s","num_turns":1,"duration_ms":5,"total_cost_usd":0.001,"stop_reason":"end_turn","permission_denials":[]}\n' "$prompt"
    ;;
  *)
    echo "Remote control is active"
    while true; do sleep 1; done
    ;;
esac
`

const fakeShellCatalog = `
models:
  - id: fake-model
    label: Fake Model
    default: true
shells:
  - id: fake
    label: Fake
    os: posix
    candidates: ["SCRIPT"]
    quote: "'"
    args: ["-c", "{{command}}"]
`

func fakeShellCatalogYAML(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}
	script := filepath.Join(t.TempDir(), "fakeshell")
	if err := os.WriteFile(script, []byte(fakeShell), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	return strings.Replace(fakeShellCatalog, "SCRIPT", script, 1)
}

type ringFixture struct {
	leader    *Server
	follower  *Server
	leaderURL string
	session   sessions.Session
}

func newRingFixture(t *testing.T) ringFixture {
	t.Helper()
	yaml := fakeShellCatalogYAML(t)

	follower := newTestServer(t, config.Config{
		Role: config.Follower, NodeID: "follower", DataDir: t.TempDir(),
		ClaudeBin: "claude", TurnTimeout: 30 * time.Second,
	}, yaml)
	followerSrv := httptest.NewServer(follower.Handler())
	t.Cleanup(followerSrv.Close)

	leader := newTestServer(t, config.Config{
		Role: config.Leader, NodeID: "leader", DataDir: t.TempDir(),
		ClaudeBin: "claude", TurnTimeout: 30 * time.Second,
		FollowersSpec: "follower|Follower|" + followerSrv.URL,
	}, yaml)
	leaderSrv := httptest.NewServer(leader.Handler())
	t.Cleanup(leaderSrv.Close)

	// Spawn on the FOLLOWER, so the leader has to resolve ownership.
	sess, err := follower.sessions.Spawn(sessions.SpawnOpts{Dir: t.TempDir(), Shell: "fake", Name: "remote"})
	if err != nil {
		t.Fatalf("spawn on follower: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := follower.sessions.Get(sess.ID); ok && s.State == "idle" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ringFixture{leader: leader, follower: follower, leaderURL: leaderSrv.URL, session: sess}
}

func do(t *testing.T, method, url string, body any) (int, []byte, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

// A leader with no ?node= resolves the follower-owned id, proxies the POST,
// and the long-polled GET returns the finished turn through the same hop.
func TestTurnSubmitAndPollThroughLeader(t *testing.T) {
	f := newRingFixture(t)
	base := f.leaderURL + "/api/sessions/" + f.session.ID

	// The single-session read must also resolve by id.
	code, body, _ := do(t, "GET", base, nil)
	if code != http.StatusOK {
		t.Fatalf("GET session = %d: %s", code, body)
	}
	var got sessions.Session
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if got.ID != f.session.ID || got.Node != "follower" {
		t.Errorf("session = %+v, want id %s on follower", got, f.session.ID)
	}

	code, body, _ = do(t, "POST", base+"/turns", sessions.TurnRequest{Prompt: "hello"})
	if code != http.StatusAccepted {
		t.Fatalf("POST turn = %d: %s", code, body)
	}
	var turn sessions.Turn
	if err := json.Unmarshal(body, &turn); err != nil {
		t.Fatalf("decode turn: %v", err)
	}
	if turn.State != "running" || turn.Node != "follower" {
		t.Fatalf("submitted turn = %+v", turn)
	}

	// Long poll through the proxy: this only works because routedByID widens
	// the proxy timeout by ?wait=.
	code, body, _ = do(t, "GET", base+"/turns/"+turn.ID+"?wait=30", nil)
	if code != http.StatusOK {
		t.Fatalf("GET turn = %d: %s", code, body)
	}
	var done sessions.Turn
	if err := json.Unmarshal(body, &done); err != nil {
		t.Fatalf("decode turn: %v", err)
	}
	if done.State != "done" {
		t.Fatalf("turn state = %q (error=%q)", done.State, done.Error)
	}
	if done.Result != "echo: hello" {
		t.Errorf("Result = %q", done.Result)
	}

	// And the list endpoint resolves by id too.
	code, body, _ = do(t, "GET", base+"/turns", nil)
	if code != http.StatusOK {
		t.Fatalf("GET turns = %d: %s", code, body)
	}
	var list []sessions.Turn
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode turns: %v", err)
	}
	if len(list) != 1 || list[0].ID != turn.ID {
		t.Errorf("turn list = %+v", list)
	}
}

// A busy 409 must arrive at the caller with its body intact — the in-flight
// turn is how a caller knows what to poll instead of blind-retrying.
func TestBusyConflictPassesThroughLeaderVerbatim(t *testing.T) {
	f := newRingFixture(t)
	base := f.leaderURL + "/api/sessions/" + f.session.ID

	code, body, _ := do(t, "POST", base+"/turns", sessions.TurnRequest{Prompt: "SLOW"})
	if code != http.StatusAccepted {
		t.Fatalf("first POST = %d: %s", code, body)
	}
	var first sessions.Turn
	json.Unmarshal(body, &first)

	code, body, hdr := do(t, "POST", base+"/turns", sessions.TurnRequest{Prompt: "second"})
	if code != http.StatusConflict {
		t.Fatalf("second POST = %d: %s", code, body)
	}
	if hdr.Get("Retry-After") != "5" {
		t.Errorf("Retry-After = %q, want 5", hdr.Get("Retry-After"))
	}
	var conflict struct {
		Error string        `json:"error"`
		State string        `json:"state"`
		Turn  sessions.Turn `json:"turn"`
	}
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if conflict.State != "busy" || conflict.Turn.ID != first.ID || conflict.Turn.State != "running" {
		t.Errorf("409 body = %s", body)
	}

	// Cancel frees the slot, and DELETE routes by id as well.
	code, body, _ = do(t, "DELETE", base+"/turns/"+first.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE turn = %d: %s", code, body)
	}
	var killed sessions.Turn
	json.Unmarshal(body, &killed)
	if killed.State != "killed" {
		t.Errorf("cancelled turn state = %q", killed.State)
	}
}

// A session whose process died is a TOMBSTONE, not a missing id: the row is
// still listed as state:"stopped" and a turn submission gets 410 Gone (stop
// retrying) rather than 409 (retry soon). That distinction has to survive the
// proxy hop verbatim.
func TestStoppedSessionGonePassesThroughLeader(t *testing.T) {
	f := newRingFixture(t)
	base := f.leaderURL + "/api/sessions/" + f.session.ID

	// Kill the session's process directly, so the row stays in the follower's
	// store (unlike DELETE, which removes it outright).
	p, err := os.FindProcess(f.session.PID)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("kill session: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body, _ := do(t, "POST", base+"/turns", sessions.TurnRequest{Prompt: "hi"})
		if code == http.StatusConflict {
			time.Sleep(50 * time.Millisecond) // still winding down
			continue
		}
		if code != http.StatusGone {
			t.Fatalf("POST to a dead session = %d: %s", code, body)
		}
		var gone struct{ Error, State string }
		if err := json.Unmarshal(body, &gone); err != nil {
			t.Fatalf("decode 410 body: %v", err)
		}
		if gone.State != "stopped" || gone.Error == "" {
			t.Errorf("410 body = %s", body)
		}
		return
	}
	t.Fatal("submitting to a dead session never returned 410")
}

// An id nobody owns is a plain 404, and must not be cached as belonging to
// anyone.
func TestUnknownSessionIDIsNotFound(t *testing.T) {
	f := newRingFixture(t)
	unknown := "11111111-2222-4333-8444-555555555555"
	for _, path := range []string{
		"/api/sessions/" + unknown,
		"/api/sessions/" + unknown + "/turns",
		"/api/sessions/" + unknown + "/log",
	} {
		if code, body, _ := do(t, "GET", f.leaderURL+path, nil); code != http.StatusNotFound {
			t.Errorf("GET %s = %d: %s", path, code, body)
		}
	}
	if _, ok := f.leader.owners.Load(unknown); ok {
		t.Error("an unknown id was cached as owned")
	}
}

// A stale id→node mapping must be evicted when the remembered node 404s,
// rather than pinning the id to a node that no longer has it.
func TestLocateCacheEvictedOn404(t *testing.T) {
	f := newRingFixture(t)

	n, ok := f.leader.locate(f.session.ID)
	if !ok || n.ID != "follower" {
		t.Fatalf("locate = %+v/%v, want follower", n, ok)
	}
	if _, cached := f.leader.owners.Load(f.session.ID); !cached {
		t.Fatal("locate did not cache the owner")
	}

	// Pretend the mapping went stale: the follower no longer has the session.
	if code, _, _ := do(t, "DELETE", f.leaderURL+"/api/sessions/"+f.session.ID, nil); code != http.StatusOK {
		t.Fatal("DELETE session failed")
	}
	if code, _, _ := do(t, "GET", f.leaderURL+"/api/sessions/"+f.session.ID, nil); code != http.StatusNotFound {
		t.Fatal("GET after removal did not 404")
	}
	if _, cached := f.leader.owners.Load(f.session.ID); cached {
		t.Error("a 404 from the owning node did not evict the cached mapping")
	}
}

// ?running=1 drops stopped rows across the whole ring.
func TestRunningFilterAcrossRing(t *testing.T) {
	f := newRingFixture(t)

	code, body, _ := do(t, "GET", f.leaderURL+"/api/sessions?running=1", nil)
	if code != http.StatusOK {
		t.Fatalf("GET sessions = %d: %s", code, body)
	}
	var rows []sessions.Session
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == f.session.ID {
			found = true
		}
		if r.State == "stopped" {
			t.Errorf("running=1 returned a stopped row: %+v", r)
		}
	}
	if !found {
		t.Errorf("running=1 dropped the live follower session: %s", body)
	}
}
