package sessions

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// Past logCap, the on-disk log stops growing (onData's write guard), but the
// UI's "recent output" tail must keep moving — otherwise TailLog freezes on
// whatever was captured in the first 256KB, exactly when a long-running
// session most needs live debugging output.
func TestTailLogStaysLivePastLogCap(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "session.log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	l := &live{
		session:  Session{StartedAt: time.Now().UnixMilli()},
		log:      f,
		answered: map[string]bool{},
	}

	// Push well past logCap.
	chunk := bytes.Repeat([]byte("x"), 8192)
	for i := 0; i < (logCap/len(chunk))+4; i++ {
		l.onData(chunk)
	}
	if l.logged < logCap {
		t.Fatalf("test setup didn't push past logCap: logged=%d", l.logged)
	}

	// Now send something distinctive and confirm it shows up in the tail —
	// proof the tail buffer isn't frozen at the logCap boundary.
	marker := []byte("DISTINCTIVE-MARKER-AFTER-CAP")
	l.onData(marker)

	l.mu.Lock()
	tail := string(l.tail)
	l.mu.Unlock()

	if !bytes.Contains([]byte(tail), marker) {
		t.Errorf("tail does not contain data written after logCap; TailLog would show stale output")
	}
	if len(tail) > tailBytes {
		t.Errorf("tail grew to %d bytes, want capped at tailBytes (%d)", len(tail), tailBytes)
	}

	// The on-disk log, by contrast, IS expected to have stopped growing at
	// (about) logCap.
	fi, err := os.Stat(f.Name())
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if fi.Size() >= logCap+int64(len(marker))+100 {
		t.Errorf("on-disk log grew past logCap (%d bytes), want it capped", fi.Size())
	}
}
