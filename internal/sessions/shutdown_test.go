package sessions

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/shells"
)

// A session whose shell ignores SIGTERM (an interactive login shell does —
// see kill_unix.go's comment) must still be gone by the time Shutdown
// returns, or a launcher restart orphans it attached to a dead PTY.
func TestShutdownWaitsOutSIGTERMIgnoringSession(t *testing.T) {
	if syscall.Kill(0, 0) != nil {
		t.Skip("requires a POSIX kill(2)")
	}

	dir := t.TempDir()
	fakeClaude := filepath.Join(dir, "fakeclaude.sh")
	script := "#!/bin/sh\ntrap '' TERM\nsleep 30\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	store, err := catalog.New("")
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	reg := shells.NewRegistry(store)
	cfg := config.Config{ClaudeBin: fakeClaude, DataDir: t.TempDir()}
	mgr := NewManager(cfg, store, reg)

	sess, err := mgr.Spawn(SpawnOpts{Dir: dir, Shell: "sh", Model: "claude-haiku-4-5-20251001", Name: "shutdown-test"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Give the shell a moment to actually exec the trap+sleep script before
	// we try to kill it.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	mgr.Shutdown()
	elapsed := time.Since(start)

	if elapsed > shutdownGrace+time.Second {
		t.Errorf("Shutdown took %s, want it bounded near shutdownGrace (%s)", elapsed, shutdownGrace)
	}

	if err := syscall.Kill(sess.PID, 0); err == nil {
		t.Errorf("process group %d still alive after Shutdown returned — session was orphaned", sess.PID)
	}
}
