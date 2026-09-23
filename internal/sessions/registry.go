package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// A live interactive `claude` process writes a small status file to
// ~/.claude/sessions/<pid>.json. Observed on 2.1.267/2.1.274/2.1.278/2.1.280:
//
//	{"pid":21578,"sessionId":"…","cwd":"…","startedAt":…,"version":"2.1.267",
//	 "kind":"interactive","status":"idle","bridgeSessionId":"session_…", …}
//
// This format is UNDOCUMENTED and version-coupled, so everything here is
// ADVISORY ONLY: it enriches the session view and lets SubmitTurn decline when
// a human is visibly driving the TUI, but no correctness guarantee ever
// depends on it. A missing directory, an unreadable file, a schema change or
// no match is never an error — callers just proceed as if nothing was found.
//
// Observed `status` values so far: "idle", "busy".
//
// A `claude -p --resume <uuid>` turn runner (this same package's own
// SubmitTurn) writes its own entry into this directory too, carrying the
// SAME sessionId as the interactive `--remote-control` process it resumes.
// Verified live against 2.1.280: that entry always carries
// entrypoint:"sdk-cli" (kind is still "interactive"), which is what
// distinguishes it from a real human-driven TUI session.
type registryEntryData struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"sessionId"`
	CWD              string `json:"cwd"`
	Status           string `json:"status"`
	Kind             string `json:"kind"`
	Entrypoint       string `json:"entrypoint"`
	Version          string `json:"version"`
	BridgeSessionID  string `json:"bridgeSessionId"`
	UpdatedAt        int64  `json:"updatedAt"`
	StatusUpdatedAt  int64  `json:"statusUpdatedAt"`
	MessagingSockPth string `json:"messagingSocketPath"`
}

// isTurnRunnerEntry reports whether a registry entry was written by this
// package's own `claude -p --resume` turn runner rather than a real
// interactive `--remote-control` session. Such entries are not a signal that
// a human is driving the session and must never gate SubmitTurn.
func (d registryEntryData) isTurnRunnerEntry() bool {
	return d.Entrypoint == "sdk-cli"
}

// registryDir is the directory holding those files, honouring CLAUDE_CONFIG_DIR.
func registryDir() string {
	base := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "sessions")
}

// registryEntry finds the live-process entry whose sessionId matches. Best
// effort: any failure returns false, never an error.
func registryEntry(sessionID string) (registryEntryData, bool) {
	if sessionID == "" {
		return registryEntryData{}, false
	}
	dir := registryDir()
	if dir == "" {
		return registryEntryData{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return registryEntryData{}, false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var d registryEntryData
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		if d.SessionID == sessionID {
			return d, true
		}
	}
	return registryEntryData{}, false
}
