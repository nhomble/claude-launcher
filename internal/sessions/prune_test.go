package sessions

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/shells"
)

// Sessions never survive a restart (in-memory only), so every log on disk at
// startup belongs to a dead session. Without pruning, data/logs grows
// forever across restarts.
func TestNewManagerPrunesOldLogs(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	old := filepath.Join(logDir, "old-session.log")
	recent := filepath.Join(logDir, "recent-session.log")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("output"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	oldTime := time.Now().Add(-logRetention - time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	store, err := catalog.New("")
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	reg := shells.NewRegistry(store)
	NewManager(config.Config{DataDir: dataDir}, store, reg)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old log %s was not pruned (err=%v)", old, err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent log %s was pruned, want kept: %v", recent, err)
	}
}
