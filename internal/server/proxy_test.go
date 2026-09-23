package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nhomble/claude-launcher/internal/nodes"
)

// pollInterval mirrors web/templates/index.html's `hx-trigger="every 4s"`.
// Kept here, not imported, since the template isn't Go-visible; changing one
// without the other should fail this test as a reminder to keep them in sync.
const pollInterval = 4 * time.Second

func TestFollowerTimeoutsStayUnderPollInterval(t *testing.T) {
	if fanoutTimeout >= pollInterval {
		t.Errorf("fanoutTimeout (%s) must stay under the UI poll interval (%s), or a dead follower causes overlapping polls to pile up", fanoutTimeout, pollInterval)
	}
	if probeTimeout >= pollInterval {
		t.Errorf("probeTimeout (%s) must stay under the UI poll interval (%s)", probeTimeout, pollInterval)
	}
}

// TestGetJSONHonorsTimeout confirms a hung follower is abandoned by
// getJSON's own deadline rather than the shared client's, so per-call
// timeouts (fanoutTimeout, probeTimeout) actually take effect.
func TestGetJSONHonorsTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never respond
	}))
	// Order matters: srv.Close() waits for in-flight handlers, so the
	// handler must be unblocked (close(block)) before we close the server.
	defer srv.Close()
	defer close(block)

	n := nodes.Node{ID: "dead", URL: srv.URL}

	start := time.Now()
	var out struct{}
	err := getJSON(n, "/healthz", nil, &out, 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("getJSON against a hung server returned nil error, want a timeout error")
	}
	if elapsed > time.Second {
		t.Errorf("getJSON took %s to time out with a 200ms deadline", elapsed)
	}
}

// An oversized body used to be silently truncated (io.LimitReader with the
// error dropped), so the follower got a mangled JSON payload and returned a
// confusing "invalid JSON body" 400 instead of the leader rejecting it
// outright.
func TestProxyRejectsOversizedBodyInsteadOfTruncating(t *testing.T) {
	followerHit := false
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followerHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer follower.Close()

	n := nodes.Node{ID: "big", URL: follower.URL}

	big := bytes.Repeat([]byte("a"), (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(string(big)))
	rec := httptest.NewRecorder()

	proxy(rec, req, n, proxyTimeout)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("proxy(oversized body) status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if followerHit {
		t.Error("proxy forwarded an oversized body to the follower instead of rejecting it")
	}
}
