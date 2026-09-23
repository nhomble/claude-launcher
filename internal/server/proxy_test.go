package server

import (
	"net/http"
	"net/http/httptest"
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
