package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/shells"
)

// The real result object captured from `claude -p --session-id <uuid> --model
// claude-haiku-4-5-20251001 --output-format json "reply with exactly: OK"` on
// 2.1.280. The parser must pick out the handful of fields the API exposes and
// ignore the several dozen it doesn't (usage, modelUsage, subagent_stats, …),
// because those change between releases.
func TestParseTurnResultRealFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "turn_result_2.1.280.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := parseTurnResult(string(b))
	if err != nil {
		t.Fatalf("parseTurnResult: %v", err)
	}
	if got.Result != "OK" {
		t.Errorf("Result = %q, want %q", got.Result, "OK")
	}
	if got.IsError {
		t.Errorf("IsError = true, want false")
	}
	if got.Subtype != "success" {
		t.Errorf("Subtype = %q, want success", got.Subtype)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", got.StopReason)
	}
	if got.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1", got.NumTurns)
	}
	if got.DurationMs <= 0 || got.CostUSD <= 0 {
		t.Errorf("DurationMs/CostUSD = %d/%v, want both > 0", got.DurationMs, got.CostUSD)
	}
	if got.SessionID != "dca11ca6-90a5-4751-bf4d-4e2a2a09ae78" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if string(got.PermissionDenials) != "[]" {
		t.Errorf("PermissionDenials = %q, want []", got.PermissionDenials)
	}
}

func TestParseTurnResultRejectsJunk(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"empty", "   "},
		// What `claude -p --resume <unknown uuid>` actually does: the message
		// goes to stderr and stdout stays empty, but be defensive anyway.
		{"plain text", "No conversation found with session ID: abc\n"},
		{"wrong type", `{"type":"system","subtype":"init"}`},
	} {
		if _, err := parseTurnResult(tc.in); err == nil {
			t.Errorf("%s: parseTurnResult succeeded, want error", tc.name)
		}
	}
}

// ── fake-shell harness ───────────────────────────────────────────────────────

// fakeShellScript stands in for a login shell running `claude`. Invoked as
// `<script> -c <command>`, it tells the two spawn paths apart by looking at
// the command:
//
//   - a turn (`-p --resume …`) reads the prompt from stdin and prints a
//     result object shaped like the real one;
//   - anything else is the interactive session: print the remote-control
//     banner (which is what flips `ready`) and then idle until killed.
const fakeShellScript = `#!/bin/sh
cmd="$2"
case "$cmd" in
  *"-p --resume"*)
    prompt=$(cat)
    case "$prompt" in
      *SLOW*) sleep 30 ;;
      *FAIL*) echo "boom: the turn failed" >&2; exit 1 ;;
    esac
    printf '{"type":"result","subtype":"success","is_error":false,"result":"echo: %s","num_turns":1,"duration_ms":5,"total_cost_usd":0.001,"stop_reason":"end_turn","permission_denials":[]}\n' "$prompt"
    ;;
  *)
    echo "Remote control is active"
    while true; do sleep 1; done
    ;;
esac
`

const fakeCatalogTemplate = `
models:
  - id: fake-model
    label: Fake Model
    default: true
shells:
  - id: fake
    label: Fake
    os: posix
    candidates: ["%s"]
    quote: "'"
    args: ["-c", "{{command}}"]
`

// newTurnTestManager builds a Manager whose only shell is the fake script, so
// these tests never touch a real `claude` binary (and never spend money).
func newTurnTestManager(t *testing.T) *Manager {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}

	// Hermetic: these tests must never read the developer's real
	// ~/.claude/sessions registry.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	dir := t.TempDir()
	script := filepath.Join(dir, "fakeshell")
	if err := os.WriteFile(script, []byte(fakeShellScript), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := strings.Replace(fakeCatalogTemplate, "%s", script, 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	store, err := catalog.New(cfgPath)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	cfg := config.Config{DataDir: filepath.Join(dir, "data"), ClaudeBin: "claude", TurnTimeout: 30 * time.Second}
	m := NewManager(cfg, store, shells.NewRegistry(store))
	t.Cleanup(m.Shutdown)
	return m
}

// spawnReady spawns a fake session and waits for the remote-control banner, so
// the session is past `starting` and will accept turns.
func spawnReady(t *testing.T, m *Manager) Session {
	t.Helper()
	sess, err := m.Spawn(SpawnOpts{Dir: t.TempDir(), Shell: "fake", Name: "test"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := m.Get(sess.ID); ok && s.State == "idle" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s never became idle", sess.ID)
	return Session{}
}

func waitTurn(t *testing.T, m *Manager, sid, tid string) Turn {
	t.Helper()
	turn, ok := m.GetTurn(sid, tid, 20*time.Second)
	if !ok {
		t.Fatalf("turn %s not found", tid)
	}
	if turn.State == "running" {
		t.Fatalf("turn %s still running after the long poll", tid)
	}
	return turn
}

// ── state machine ────────────────────────────────────────────────────────────

func TestSubmitTurnRoundTrip(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "hello"})
	if err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}
	if turn.State != "running" || turn.SessionID != sess.ID {
		t.Fatalf("submitted turn = %+v", turn)
	}

	done := waitTurn(t, m, sess.ID, turn.ID)
	if done.State != "done" {
		t.Fatalf("turn state = %q (error=%q)", done.State, done.Error)
	}
	if done.Result != "echo: hello" {
		t.Errorf("Result = %q, want %q", done.Result, "echo: hello")
	}
	// The slot must be free again, and the finished turn must be in the ring.
	if s, _ := m.Get(sess.ID); s.State != "idle" || s.TurnID != "" {
		t.Errorf("after completion session = %q/%q, want idle with no turnId", s.State, s.TurnID)
	}
	turns, ok := m.ListTurns(sess.ID)
	if !ok || len(turns) != 1 || turns[0].ID != turn.ID {
		t.Errorf("ListTurns = %+v", turns)
	}
}

// A turn whose process fails settles to state "error" carrying the stderr
// tail — the failure is only knowable after exec, so it is deliberately NOT
// folded into the submit-time decision.
func TestSubmitTurnProcessFailure(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "please FAIL"})
	if err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}
	done := waitTurn(t, m, sess.ID, turn.ID)
	if done.State != "error" {
		t.Fatalf("state = %q, want error", done.State)
	}
	if !strings.Contains(done.Error, "boom") {
		t.Errorf("Error = %q, want the stderr tail", done.Error)
	}
	if s, _ := m.Get(sess.ID); s.State != "idle" {
		t.Errorf("a failed turn left the session %q, want idle", s.State)
	}
}

// THE core invariant: a session runs one turn at a time, ever. Many concurrent
// submissions must produce exactly one acceptance and N-1 ErrBusy.
//
// Regression check performed by hand: replacing the check-and-set in
// SubmitTurn with an unsynchronised "read turn; if nil, assign" (i.e. dropping
// the single l.mu section) makes this test fail with several accepted turns.
func TestSubmitTurnSingleSlotUnderConcurrency(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	const n = 32
	var (
		mu       sync.Mutex
		accepted []Turn
		busy     int
		other    []error
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// SLOW keeps the turn running long enough that every other
			// submission lands while the slot is held.
			turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "SLOW"})
			mu.Lock()
			defer mu.Unlock()
			switch e := err.(type) {
			case nil:
				accepted = append(accepted, turn)
			case *ErrBusy:
				busy++
				if e.Turn.ID == "" || e.Turn.State != "running" {
					other = append(other, e)
				}
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(accepted) != 1 {
		t.Fatalf("accepted %d turns, want exactly 1", len(accepted))
	}
	if busy != n-1 {
		t.Errorf("got %d ErrBusy, want %d (other errors: %v)", busy, n-1, other)
	}
	if len(other) != 0 {
		t.Errorf("unexpected errors: %v", other)
	}

	// And the session reports itself busy with that exact turn.
	s, _ := m.Get(sess.ID)
	if s.State != "busy" || s.TurnID != accepted[0].ID {
		t.Errorf("session = %q/%q, want busy/%s", s.State, s.TurnID, accepted[0].ID)
	}

	// Cancelling frees the slot again.
	cancelled, err := m.CancelTurn(sess.ID, accepted[0].ID)
	if err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
	if cancelled.State != "killed" {
		t.Errorf("cancelled turn state = %q, want killed", cancelled.State)
	}
	if s, _ := m.Get(sess.ID); s.State != "idle" {
		t.Errorf("after cancel session = %q, want idle", s.State)
	}
}

// writeRegistryEntry drops a synthetic ~/.claude/sessions/<pid>.json (under
// the test's isolated CLAUDE_CONFIG_DIR) so SubmitTurn's advisory check has
// something to read.
func writeRegistryEntry(t *testing.T, pid int, sessionID, status, entrypoint string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir registry dir: %v", err)
	}
	entry := registryEntryData{
		PID:        pid,
		SessionID:  sessionID,
		Status:     status,
		Kind:       "interactive",
		Entrypoint: entrypoint,
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal registry entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), b, 0o644); err != nil {
		t.Fatalf("write registry entry: %v", err)
	}
}

// A stale/live interactive registry entry reporting status:"busy" makes
// SubmitTurn decline with ErrRemoteBusy — the review found there was no test
// for this gating path at all.
func TestSubmitTurnRejectsRemoteBusy(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	writeRegistryEntry(t, 999901, sess.ID, "busy", "")

	_, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "hi"})
	var remoteBusy *ErrRemoteBusy
	if !errors.As(err, &remoteBusy) {
		t.Fatalf("SubmitTurn = %v, want *ErrRemoteBusy", err)
	}
	if remoteBusy.Status != "busy" {
		t.Errorf("ErrRemoteBusy.Status = %q, want busy", remoteBusy.Status)
	}
}

// A registry entry that looks like this package's own `claude -p --resume`
// turn runner (entrypoint:"sdk-cli", verified live against 2.1.280) must
// never gate SubmitTurn — otherwise a turn killed by SIGKILL (which leaves
// its own busy entry behind, see kill_unix.go's escalation) would lock the
// session out of every future turn.
func TestSubmitTurnIgnoresOwnTurnRunnerEntry(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	writeRegistryEntry(t, 999902, sess.ID, "busy", "sdk-cli")

	turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("SubmitTurn wrongly gated on our own turn runner's registry entry: %v", err)
	}
	waitTurn(t, m, sess.ID, turn.ID)
}

// An unknown/future status value must fail OPEN, not closed: the registry
// format is explicitly undocumented and version-coupled (see registry.go),
// so only the one known-bad value ("busy") may gate a turn.
func TestSubmitTurnAllowsUnknownRegistryStatus(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	writeRegistryEntry(t, 999903, sess.ID, "compacting", "")

	turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("SubmitTurn wrongly gated on an unknown registry status: %v", err)
	}
	waitTurn(t, m, sess.ID, turn.ID)
}

func TestSubmitTurnRejectsStoppedSession(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	l, ok := m.get(sess.ID)
	if !ok {
		t.Fatal("session vanished")
	}
	killGroup(l.cmd.Process)
	<-l.reaped

	if _, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "hi"}); err != ErrStopped {
		t.Fatalf("SubmitTurn on a stopped session = %v, want ErrStopped", err)
	}
	if s, _ := m.Get(sess.ID); s.State != "stopped" {
		t.Errorf("state = %q, want stopped", s.State)
	}
}

// Before remote control is up, resuming would hit "No conversation found" —
// so submissions inside the startup window are refused outright.
func TestSubmitTurnRejectsStartingSession(t *testing.T) {
	m := newTurnTestManager(t)
	l := &live{
		session:  Session{ID: newUUID(), StartedAt: time.Now().UnixMilli(), Shell: "fake"},
		answered: map[string]bool{},
		pumped:   make(chan struct{}),
		reaped:   make(chan struct{}),
	}
	m.mu.Lock()
	m.items[l.session.ID] = l
	m.mu.Unlock()

	if _, err := m.SubmitTurn(l.session.ID, TurnRequest{Prompt: "hi"}); err != ErrStarting {
		t.Fatalf("SubmitTurn = %v, want ErrStarting", err)
	}
	if s, _ := m.Get(l.session.ID); s.State != "starting" {
		t.Errorf("state = %q, want starting", s.State)
	}

	// Drop it before Shutdown, which would otherwise wait on a session that
	// has no real process behind it.
	m.mu.Lock()
	delete(m.items, l.session.ID)
	m.mu.Unlock()
}

func TestSubmitTurnUnknownSessionAndBadRequests(t *testing.T) {
	m := newTurnTestManager(t)
	if _, err := m.SubmitTurn("nope", TurnRequest{Prompt: "hi"}); err != ErrNotFound {
		t.Errorf("unknown id = %v, want ErrNotFound", err)
	}

	sess := spawnReady(t, m)
	for _, tc := range []struct {
		name string
		req  TurnRequest
	}{
		{"empty prompt", TurnRequest{Prompt: "   "}},
		{"bad permission mode", TurnRequest{Prompt: "hi", PermissionMode: "yolo"}},
		{"negative timeout", TurnRequest{Prompt: "hi", TimeoutSec: -1}},
	} {
		_, err := m.SubmitTurn(sess.ID, tc.req)
		var bad ErrBadRequest
		if err == nil || !errorsAs(err, &bad) {
			t.Errorf("%s: err = %v, want ErrBadRequest", tc.name, err)
		}
	}
	// Every allowlisted mode from `claude -p --help` on 2.1.280 is accepted.
	for _, mode := range permissionModes {
		turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "hi", PermissionMode: mode})
		if err != nil {
			t.Fatalf("permissionMode %q rejected: %v", mode, err)
		}
		waitTurn(t, m, sess.ID, turn.ID)
	}
}

func errorsAs(err error, target *ErrBadRequest) bool {
	e, ok := err.(ErrBadRequest)
	if ok {
		*target = e
	}
	return ok
}

// A turn must never outlive the session it belongs to.
func TestRemoveKillsRunningTurn(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "SLOW"})
	if err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}
	l, _ := m.get(sess.ID)

	// Wait for the runner to actually have a process to kill.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		started := l.turnCmd != nil
		l.mu.Unlock()
		if started {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !m.Remove(sess.ID) {
		t.Fatal("Remove returned false")
	}
	select {
	case <-l.turnDoneFor(turn.ID):
	case <-time.After(10 * time.Second):
		t.Fatal("the turn outlived Remove")
	}
	l.mu.Lock()
	state := l.findTurnLocked(turn.ID).State
	l.mu.Unlock()
	if state != "killed" {
		t.Errorf("turn state after Remove = %q, want killed", state)
	}
}

// turnDoneFor exposes a turn's completion channel to the tests.
func (l *live) turnDoneFor(id string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := l.findTurnLocked(id); t != nil {
		return t.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

func TestTurnTimeout(t *testing.T) {
	m := newTurnTestManager(t)
	sess := spawnReady(t, m)

	turn, err := m.SubmitTurn(sess.ID, TurnRequest{Prompt: "SLOW", TimeoutSec: 1})
	if err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}
	done := waitTurn(t, m, sess.ID, turn.ID)
	if done.State != "timeout" {
		t.Fatalf("state = %q, want timeout", done.State)
	}
	if !strings.Contains(done.Error, "timeout after") {
		t.Errorf("Error = %q", done.Error)
	}
}

// Turn JSON must round-trip through the API's own encoding — the server hands
// these straight to writeJSON.
func TestTurnJSONShape(t *testing.T) {
	turn := Turn{ID: "t", SessionID: "s", State: "done", Result: "ok", PermissionDenials: json.RawMessage(`[]`)}
	b, err := json.Marshal(turn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "sessionId", "state", "result", "isError"} {
		if _, ok := back[k]; !ok {
			t.Errorf("marshalled turn is missing %q: %s", k, b)
		}
	}
}
