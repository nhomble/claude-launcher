package server

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/nodes"
	"github.com/nhomble/claude-launcher/internal/sessions"
	"github.com/nhomble/claude-launcher/internal/shells"
)

func newTestServer(t *testing.T, cfg config.Config, catalogYAML string) *Server {
	t.Helper()

	catalogPath := ""
	if catalogYAML != "" {
		catalogPath = filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(catalogPath, []byte(catalogYAML), 0o644); err != nil {
			t.Fatalf("write catalog: %v", err)
		}
	}
	store, err := catalog.New(catalogPath)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}

	ring, err := nodes.New(cfg)
	if err != nil {
		t.Fatalf("nodes.New: %v", err)
	}

	reg := shells.NewRegistry(store)
	mgr := sessions.NewManager(cfg, store, reg)
	t.Cleanup(mgr.Shutdown)

	srv, err := New(cfg, ring, mgr, store, reg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv
}

// followerOnlyCatalog offers a model the leader's default catalog does not.
const followerOnlyCatalog = `
models:
  - id: follower-only-model
    label: Follower-Only Model
    default: true
shells:
  - id: sh
    label: sh
    os: posix
    candidates: [sh]
    quote: "'"
    args: ["-c", "{{command}}"]
`

// A leader's model dropdown must reflect the TARGETED node's own catalog, not
// always its own: launch-time validation (sessions.Manager.Spawn) resolves
// against the target node's catalog, so a UI showing the leader's list can
// offer ids the follower rejects, or hide ones only the follower supports.
func TestAPIModelsRoutedToTargetNode(t *testing.T) {
	follower := newTestServer(t, config.Config{Role: config.Follower, NodeID: "follower"}, followerOnlyCatalog)
	followerSrv := httptest.NewServer(follower.Handler())
	defer followerSrv.Close()

	leader := newTestServer(t, config.Config{
		Role:          config.Leader,
		NodeID:        "leader",
		FollowersSpec: "follower|follower|" + followerSrv.URL,
	}, "")

	// Self (leader) keeps its own catalog: the default model list, not the
	// follower's.
	self, err := leader.modelsFor(leader.ring.Self())
	if err != nil {
		t.Fatalf("modelsFor(self): %v", err)
	}
	assertHasModel(t, self, "claude-opus-5")

	// Routed to the follower, the leader must see the follower's own models.
	n, ok := leader.ring.Get("follower")
	if !ok {
		t.Fatal("leader ring missing follower")
	}
	remote, err := leader.modelsFor(n)
	if err != nil {
		t.Fatalf("modelsFor(follower): %v", err)
	}
	assertHasModel(t, remote, "follower-only-model")
	for _, m := range remote {
		if m.ID == "claude-opus-5" {
			t.Errorf("follower catalog leaked the leader's claude-opus-5 model")
		}
	}

	// The actual regression: GET /api/models?node=follower on the leader's
	// own HTTP mux (what the UI's host-panel fragment now fetches through)
	// must proxy to the follower rather than silently answering locally.
	leaderSrv := httptest.NewServer(leader.Handler())
	defer leaderSrv.Close()

	var viaHTTP []catalog.Model
	q := url.Values{"node": {"follower"}}
	if err := getJSON(nodes.Node{URL: leaderSrv.URL}, "/api/models", q, &viaHTTP, proxyTimeout); err != nil {
		t.Fatalf("GET /api/models?node=follower: %v", err)
	}
	assertHasModel(t, viaHTTP, "follower-only-model")
}

func assertHasModel(t *testing.T, models []catalog.Model, id string) {
	t.Helper()
	for _, m := range models {
		if m.ID == id {
			return
		}
	}
	t.Errorf("model %q not found in %+v", id, models)
}
