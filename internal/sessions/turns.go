package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A TURN is one prompt submitted to an existing session over the API, and the
// result that comes back.
//
// A turn does NOT go through the session's PTY. The interactive
// `--remote-control` process is left completely alone: instead the launcher
// runs a separate, short-lived
//
//	claude -p --resume <session uuid> --output-format json
//
// in the session's working directory, with the prompt on stdin. That works
// because Spawn pre-assigns the session's uuid via `--session-id`, so the
// interactive session and the turn runner are talking about the same
// transcript. Verified live against claude 2.1.280: resuming while the
// remote-control process is still connected succeeds, appends LINEARLY to the
// transcript (no branching), and leaves the interactive process healthy.
//
// Two known caveats, both documented in the README:
//   - the live TUI does not re-render to show an API-submitted turn; it only
//     sees it after a resume/restart.
//   - a human typing into the TUI at the same instant an API turn runs has NOT
//     been verified. If real branching is ever observed there, the fallback is
//     `--fork-session` on the first API turn and chaining later API turns to
//     that fork's id. That path is deliberately NOT implemented — it wasn't
//     needed in any tested scenario.
//
// Concurrency: a session runs at most one turn at a time, enforced by the
// single `live.turn` slot checked-and-set under l.mu in SubmitTurn. There must
// never be a second code path that starts a turn.

const (
	maxPromptBytes = 1 << 20 // 1 MiB
	maxTurnTimeout = 60 * time.Minute
	maxStdout      = 4 << 20
	maxStderr      = 64 << 10
	turnRingSize   = 20

	// Used when the config carries no TurnTimeout (zero value), e.g. a
	// Manager built in a test with a bare config.Config.
	defaultTurnTimeout = 15 * time.Minute
)

// permissionModes is the allowlist for TurnRequest.PermissionMode, taken
// verbatim from `claude -p --help` on 2.1.280.
var permissionModes = []string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"}

type TurnRequest struct {
	Prompt         string  `json:"prompt"`                   // required, non-empty, <= 1 MiB
	PermissionMode string  `json:"permissionMode,omitempty"` // must be in permissionModes
	TimeoutSec     int     `json:"timeoutSec,omitempty"`     // 0 -> cfg.TurnTimeout; capped at 60m
	MaxBudgetUSD   float64 `json:"maxBudgetUsd,omitempty"`   // > 0 -> --max-budget-usd
}

type Turn struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Node      string `json:"node,omitempty"` // filled by the server layer from ring.Self()
	Prompt    string `json:"prompt"`
	// "running" | "done" | "error" | "timeout" | "killed"
	State      string `json:"state"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`

	// From claude's result object (State == "done").
	Result            string          `json:"result,omitempty"`
	IsError           bool            `json:"isError"`
	Subtype           string          `json:"subtype,omitempty"`
	StopReason        string          `json:"stopReason,omitempty"`
	NumTurns          int             `json:"numTurns,omitempty"`
	DurationMs        int64           `json:"durationMs,omitempty"`
	CostUSD           float64         `json:"costUsd,omitempty"`
	PermissionDenials json.RawMessage `json:"permissionDenials,omitempty"`

	// Launcher-side failure detail (State != "done").
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`

	done      chan struct{} // closed on completion; GetTurn's long poll selects on it
	cancelled bool          // guarded by live.mu; set by CancelTurn/killTurnLocked
}

// ── typed errors, so the server can map cleanly to status codes ─────────────

var (
	ErrNotFound = errors.New("not found")
	ErrStopped  = errors.New("session has stopped")
	ErrStarting = errors.New("session still starting")
)

// ErrBusy carries the in-flight turn so a caller can poll it instead of
// blind-retrying.
type ErrBusy struct{ Turn Turn }

func (e *ErrBusy) Error() string { return "session busy" }

// ErrRemoteBusy means the advisory ~/.claude/sessions registry says a human is
// driving this session right now.
type ErrRemoteBusy struct{ Status string }

func (e *ErrRemoteBusy) Error() string {
	return fmt.Sprintf("session is being driven remotely (status=%q)", e.Status)
}

type ErrBadRequest string

func (e ErrBadRequest) Error() string { return string(e) }

// ── manager API ─────────────────────────────────────────────────────────────

func (m *Manager) get(id string) (*live, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.items[id]
	return l, ok
}

// Get returns one session's snapshot.
func (m *Manager) Get(id string) (Session, bool) {
	l, ok := m.get(id)
	if !ok {
		return Session{}, false
	}
	return l.snapshot(), true
}

// SubmitTurn validates the request, claims the session's single turn slot and
// starts the runner. It returns as soon as the process is launched — the
// result is collected by GetTurn.
func (m *Manager) SubmitTurn(id string, req TurnRequest) (Turn, error) {
	l, ok := m.get(id)
	if !ok {
		return Turn{}, ErrNotFound
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Turn{}, ErrBadRequest("prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return Turn{}, ErrBadRequest(fmt.Sprintf("prompt exceeds %d bytes", maxPromptBytes))
	}
	if req.PermissionMode != "" && !slices.Contains(permissionModes, req.PermissionMode) {
		return Turn{}, ErrBadRequest("unknown permissionMode: " + req.PermissionMode +
			" (want one of " + strings.Join(permissionModes, ", ") + ")")
	}
	if req.TimeoutSec < 0 {
		return Turn{}, ErrBadRequest("timeoutSec must not be negative")
	}
	timeout := m.cfg.TurnTimeout
	if req.TimeoutSec > 0 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}
	if timeout <= 0 {
		timeout = defaultTurnTimeout
	}
	if timeout > maxTurnTimeout {
		timeout = maxTurnTimeout
	}

	// The launcher's own authoritative in-memory state always takes
	// precedence over the best-effort filesystem signal below: a stopped
	// session must report ErrStopped (not a misleading "retry later"), and a
	// session busy with our OWN in-flight turn must return that turn so the
	// caller can poll it.
	if err := l.peekTurnGate(); err != nil {
		return Turn{}, err
	}

	// The advisory check happens OUTSIDE l.mu (it touches the filesystem),
	// and only after the launcher's own state says the session is otherwise
	// available for a turn. It can only ever add a rejection on top of that,
	// never override a rejection the slot check already made. Skip entries
	// written by this package's own `claude -p --resume` turn runner: they
	// carry the same sessionId as the interactive session but are not a
	// human driving the TUI, so they must never gate SubmitTurn (a killed
	// turn runner can leave one of these behind indefinitely).
	if e, ok := registryEntry(id); ok && e.Status == "busy" && !e.isTurnRunnerEntry() {
		return Turn{}, &ErrRemoteBusy{Status: e.Status}
	}

	l.mu.Lock()
	switch {
	case l.exited:
		l.mu.Unlock()
		return Turn{}, ErrStopped
	case l.turn != nil:
		busy := *l.turn
		l.mu.Unlock()
		return Turn{}, &ErrBusy{Turn: busy}
	case !l.ready && time.Since(time.UnixMilli(l.session.StartedAt)) < gateWindow:
		l.mu.Unlock()
		return Turn{}, ErrStarting
	}
	t := &Turn{
		ID:        newUUID(),
		SessionID: id,
		Prompt:    prompt,
		State:     "running",
		StartedAt: time.Now().UnixMilli(),
		done:      make(chan struct{}),
	}
	l.turn = t
	out := *t
	dir, model := l.session.Dir, l.session.Model
	l.mu.Unlock()

	go l.runTurn(m, t, req, dir, model, timeout)
	return out, nil
}

// peekTurnGate re-checks the launcher's own state (the same switch
// SubmitTurn will do again once it actually claims the slot) so the advisory
// registry check below is skipped entirely when the launcher's own state
// would already refuse the turn. A nil return means the session currently
// looks available; a non-nil return is the launcher's own, authoritative
// outcome and must be returned to the caller as-is.
func (l *live) peekTurnGate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.exited:
		return ErrStopped
	case l.turn != nil:
		return &ErrBusy{Turn: *l.turn}
	case !l.ready && time.Since(time.UnixMilli(l.session.StartedAt)) < gateWindow:
		return ErrStarting
	}
	return nil
}

// GetTurn returns a turn, optionally long-polling up to wait for it to finish.
func (m *Manager) GetTurn(id, turnID string, wait time.Duration) (Turn, bool) {
	l, ok := m.get(id)
	if !ok {
		return Turn{}, false
	}
	l.mu.Lock()
	t := l.findTurnLocked(turnID)
	l.mu.Unlock()
	if t == nil {
		return Turn{}, false
	}

	if wait > 0 {
		select {
		case <-t.done:
		case <-time.After(wait):
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return *t, true
}

// ListTurns returns the in-flight turn (if any) plus the finished ring,
// newest first.
func (m *Manager) ListTurns(id string) ([]Turn, bool) {
	l, ok := m.get(id)
	if !ok {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Turn, 0, len(l.turns)+1)
	if l.turn != nil {
		out = append(out, *l.turn)
	}
	for i := len(l.turns) - 1; i >= 0; i-- {
		out = append(out, *l.turns[i])
	}
	return out, true
}

// CancelTurn kills a running turn's process group. A turn that already
// finished is returned unchanged alongside ErrBadRequest so the caller can see
// its outcome.
func (m *Manager) CancelTurn(id, turnID string) (Turn, error) {
	l, ok := m.get(id)
	if !ok {
		return Turn{}, ErrNotFound
	}
	l.mu.Lock()
	t := l.findTurnLocked(turnID)
	if t == nil {
		l.mu.Unlock()
		return Turn{}, ErrNotFound
	}
	if t.State != "running" {
		fin := *t
		l.mu.Unlock()
		return fin, ErrBadRequest("turn already finished")
	}
	t.cancelled = true
	cmd := l.turnCmd
	l.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		killGroup(cmd.Process)
	}
	<-t.done

	l.mu.Lock()
	defer l.mu.Unlock()
	return *t, nil
}

// findTurnLocked looks in the single slot and then the finished ring.
func (l *live) findTurnLocked(turnID string) *Turn {
	if l.turn != nil && l.turn.ID == turnID {
		return l.turn
	}
	for _, t := range l.turns {
		if t.ID == turnID {
			return t
		}
	}
	return nil
}

// ── the runner ──────────────────────────────────────────────────────────────

// capBuf is a bounded io.Writer: it keeps the first max bytes and counts the
// rest, so a runaway turn can never balloon the launcher's memory.
type capBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (l *live) runTurn(m *Manager, t *Turn, req TurnRequest, dir, model string, timeout time.Duration) {
	sh, ok := m.shells.Get(l.session.Shell)
	if !ok {
		l.finishTurn(t, "error", func(t *Turn) {
			t.Error = "shell " + l.session.Shell + " is no longer available"
		})
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s -p --resume %s%s%s --model %s%s%s --output-format json",
		m.cfg.ClaudeBin, sh.Quote, t.SessionID, sh.Quote, sh.Quote, model, sh.Quote)
	if req.PermissionMode != "" {
		// Already allowlisted above, so it cannot carry shell metacharacters.
		fmt.Fprintf(&b, " --permission-mode %s", req.PermissionMode)
	}
	if req.MaxBudgetUSD > 0 {
		fmt.Fprintf(&b, " --max-budget-usd %s", strconv.FormatFloat(req.MaxBudgetUSD, 'f', -1, 64))
	}

	cmd := exec.Command(sh.Bin, sh.Argv(b.String())...)
	cmd.Dir = dir
	// Same marker stripping as the interactive spawn path: a turn that
	// inherited CLAUDE_CODE_* would believe it is a CHILD of the launcher's
	// own claude session and silently stop persisting its transcript.
	cmd.Env, _ = sessionEnv()
	// The prompt goes over stdin so it never has to be shell-quoted.
	cmd.Stdin = strings.NewReader(t.Prompt)
	stdout := &capBuf{max: maxStdout}
	stderr := &capBuf{max: maxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setGroup(cmd)

	if err := cmd.Start(); err != nil {
		l.finishTurn(t, "error", func(t *Turn) { t.Error = "cannot start turn: " + err.Error() })
		return
	}

	l.mu.Lock()
	l.turnCmd = cmd
	// A cancel that arrived between the slot claim and cmd.Start() found
	// turnCmd still nil and could only set the flag — so honour it here, or
	// the turn would run on to its timeout and be reported as one.
	alreadyCancelled := t.cancelled
	l.mu.Unlock()
	if alreadyCancelled && cmd.Process != nil {
		killGroup(cmd.Process)
	}

	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		l.mu.Lock()
		timedOut = true
		l.mu.Unlock()
		if cmd.Process != nil {
			killGroup(cmd.Process)
		}
	})

	err := cmd.Wait()
	timer.Stop()

	l.mu.Lock()
	l.turnCmd = nil
	hitTimeout, cancelled := timedOut, t.cancelled
	l.mu.Unlock()

	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	} else if err != nil {
		code = -1
	}

	switch {
	// An explicit cancel outranks the timeout: if both fired, the caller
	// asked for this and should be told "killed", not "timeout".
	case cancelled:
		l.finishTurn(t, "killed", func(t *Turn) {
			t.ExitCode = code
			t.Error = "cancelled"
		})
	case hitTimeout:
		l.finishTurn(t, "timeout", func(t *Turn) {
			t.ExitCode = code
			t.Error = "timeout after " + timeout.String()
		})
	default:
		res, perr := parseTurnResult(stdout.String())
		if err != nil || perr != nil {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = strings.TrimSpace(stdout.String())
			}
			if detail == "" && perr != nil {
				detail = perr.Error()
			}
			l.finishTurn(t, "error", func(t *Turn) {
				t.ExitCode = code
				t.Error = tailN(detail, maxStderr)
			})
			return
		}
		l.finishTurn(t, "done", func(t *Turn) {
			t.ExitCode = code
			t.Result = res.Result
			t.IsError = res.IsError
			t.Subtype = res.Subtype
			t.StopReason = res.StopReason
			t.NumTurns = res.NumTurns
			t.DurationMs = res.DurationMs
			t.CostUSD = res.CostUSD
			t.PermissionDenials = res.PermissionDenials
		})
	}
}

// finishTurn records the outcome, frees the single slot and wakes long pollers.
// It is the ONLY place a turn leaves the "running" state.
func (l *live) finishTurn(t *Turn, state string, fill func(*Turn)) {
	l.mu.Lock()
	t.State = state
	t.FinishedAt = time.Now().UnixMilli()
	if fill != nil {
		fill(t)
	}
	l.turns = append(l.turns, t)
	if len(l.turns) > turnRingSize {
		l.turns = l.turns[len(l.turns)-turnRingSize:]
	}
	if l.turn == t {
		l.turn = nil
	}
	sess, dur := l.session.ID, time.Since(time.UnixMilli(t.StartedAt)).Round(time.Millisecond)
	l.mu.Unlock()

	log.Printf("session %s turn %s %s after %s", sess, t.ID, state, dur)
	close(t.done)
}

// killTurnLocked kills an in-flight turn's process. Caller holds l.mu; the
// runner records the turn as "killed" once the process actually dies.
func (l *live) killTurnLocked() {
	if l.turn != nil {
		l.turn.cancelled = true
	}
	if l.turnCmd != nil && l.turnCmd.Process != nil {
		killGroup(l.turnCmd.Process)
	}
}

// ── result parsing ──────────────────────────────────────────────────────────

// turnResult is the subset of claude's `--output-format json` result object
// the launcher cares about. Deliberately permissive: the real object carries
// dozens of fields (usage, modelUsage, subagent_stats, …) that change between
// releases, and unknown fields are simply ignored.
type turnResult struct {
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	IsError           bool            `json:"is_error"`
	Result            string          `json:"result"`
	StopReason        string          `json:"stop_reason"`
	NumTurns          int             `json:"num_turns"`
	DurationMs        int64           `json:"duration_ms"`
	CostUSD           float64         `json:"total_cost_usd"`
	SessionID         string          `json:"session_id"`
	PermissionDenials json.RawMessage `json:"permission_denials"`
}

func parseTurnResult(stdout string) (turnResult, error) {
	s := strings.TrimSpace(stdout)
	if s == "" {
		return turnResult{}, errors.New("turn produced no output")
	}
	var r turnResult
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return turnResult{}, fmt.Errorf("turn output is not JSON: %w", err)
	}
	if r.Type != "result" {
		return turnResult{}, fmt.Errorf("unexpected turn output type %q", r.Type)
	}
	return r, nil
}

// tailN keeps the LAST n bytes — the end of a stderr stream is where the
// actual failure is.
func tailN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
