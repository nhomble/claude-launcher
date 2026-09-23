package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

// The registry reader is ADVISORY: it must find a matching entry when one is
// there, and must never turn a missing directory, a junk file or an unrelated
// entry into anything other than "not found".
func TestRegistryEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	// Shape captured from a real ~/.claude/sessions/<pid>.json (2.1.278).
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("80475.json", `{"pid":80475,"sessionId":"e8d4fc32-82e0-4199-a6b3-6747cfab1759","cwd":"/tmp",
		"startedAt":1,"version":"2.1.278","kind":"interactive","status":"busy","updatedAt":2,
		"bridgeSessionId":"session_01CYkViuanA6RpqQa7UEiUaK","messagingSocketPath":"/tmp/cc-socks/80475.sock","peerProtocol":1}`)
	write("57010.json", `{"pid":57010,"sessionId":"0c5afe2e-b75f-4cdf-9351-c859c1e0af44","status":"idle","version":"2.1.274"}`)
	write("junk.json", `not json at all`)
	write("ignored.key", `binary-ish`)

	got, ok := registryEntry("e8d4fc32-82e0-4199-a6b3-6747cfab1759")
	if !ok {
		t.Fatal("registryEntry: no match, want the 80475 entry")
	}
	if got.PID != 80475 || got.Status != "busy" || got.Version != "2.1.278" ||
		got.BridgeSessionID != "session_01CYkViuanA6RpqQa7UEiUaK" {
		t.Errorf("entry = %+v", got)
	}

	if _, ok := registryEntry("11111111-2222-4333-8444-555555555555"); ok {
		t.Error("registryEntry matched an unknown session id")
	}
	if _, ok := registryEntry(""); ok {
		t.Error("registryEntry matched the empty session id")
	}

	// A missing directory is not an error, just a miss.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "does-not-exist"))
	if _, ok := registryEntry("e8d4fc32-82e0-4199-a6b3-6747cfab1759"); ok {
		t.Error("registryEntry matched with no registry directory")
	}
}

func TestNewUUIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newUUID()
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("malformed uuid %q", id)
		}
		if id[14] != '4' {
			t.Fatalf("uuid %q is not version 4", id)
		}
		if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("uuid %q has the wrong variant nibble %q", id, v)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid %q", id)
		}
		seen[id] = true
	}
}
