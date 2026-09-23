package sessions

import (
	"os"
	"testing"
	"time"
)

// newTestLive builds a bare live for exercising scanGatesLocked directly —
// no real pty/cmd/log needed, since matching only touches plain fields.
func newTestLive() *live {
	return &live{
		session:  Session{StartedAt: time.Now().UnixMilli()},
		answered: map[string]bool{},
	}
}

// The fixture is a raw PTY capture (ANSI included) of `claude --remote-control`
// hitting the per-directory trust dialog on a fresh, never-trusted directory —
// recorded live against claude 2.1.280 on 2026-09-22. Its rendering jumps the
// cursor to an absolute column between words instead of printing spaces, so
// stripANSI collapses words together ("Isthisaprojectyoucreatedoroneyoutrust")
// — this is what broke the old literal-space regexes.
func loadTrustDialogFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/trust_dialog_2.1.280.bin")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestFolderTrustGateMatchesRealCapture(t *testing.T) {
	l := newTestLive()
	hits := l.scanGatesLocked(loadTrustDialogFixture(t))

	if len(hits) != 1 || hits[0].id != "folder-trust" {
		t.Fatalf("scanGatesLocked(real trust-dialog capture) = %+v, want one folder-trust hit", hits)
	}
	// Live-verified: the dialog's default-focused option is "No, exit", not
	// "Yes, I trust this folder" — a bare "\r" would exit the session. The
	// fix must select the other option first.
	if hits[0].send != "\x1b[B\r" {
		t.Errorf("folder-trust Send = %q, want Down+Enter (\"\\x1b[B\\r\") to move off the cancel-focused default", hits[0].send)
	}
}

func TestGateRegexesToleratePTYCursorJumpConcatenation(t *testing.T) {
	// Representative post-stripANSI text for each known gate, exactly as the
	// real renderer produces it: words joined with no separating space.
	cases := map[string]string{
		"folder-trust":        "QuicksafetycheckIsthisaprojectyoucreatedoroneyoutrust",
		"chrome-extension":    "TheClaudeinChromeextensiondetectedwouldyouliketouseit",
		"fullscreen-renderer": "Wanttotrythenewfullscreenrendererforabettercodingexperience",
	}
	for id, text := range cases {
		l := newTestLive()
		hits := l.scanGatesLocked([]byte(text))
		if len(hits) != 1 || hits[0].id != id {
			t.Errorf("scanGatesLocked(%q) = %+v, want a single %s hit", text, hits, id)
		}
	}
}

func TestGatesDoneToleratesConcatenation(t *testing.T) {
	if !gatesDone.MatchString("rc/remote-controlisactiveContinuehere") {
		t.Error("gatesDone regex regressed: no longer tolerates zero-space concatenation")
	}
}
