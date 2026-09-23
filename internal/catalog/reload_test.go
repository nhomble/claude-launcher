package catalog

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestReloadLogsWhenConfigDisappears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, defaultsYAML, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// mtime/size resolution can be coarse; force a distinct stamp state
	// by removing rather than editing, which stampOf reports as "".
	s.reloadIfChanged()

	if !bytes.Contains(buf.Bytes(), []byte("disappeared")) {
		t.Errorf("expected a \"disappeared\" log line after removing %s, got: %q", path, buf.String())
	}

	// The catalog itself must still serve the last-good models.
	if len(s.Models()) == 0 {
		t.Error("Models() empty after config disappeared; want last-good defaults retained")
	}

	// A second reload with the file still gone must not log again (log-once).
	buf.Reset()
	s.reloadIfChanged()
	if buf.Len() != 0 {
		t.Errorf("expected no repeat log on unchanged disappeared state, got: %q", buf.String())
	}
}
