// Package sessions is the session store.
//
// `claude --remote-control` is an INTERACTIVE command — given a non-TTY stdout
// it drops into --print mode and exits ("Input must be provided …"). So each
// session is spawned through a real pseudo-terminal (ConPTY on Windows,
// forkpty on macOS/Linux). The PTY is windowless but gives claude the terminal
// it needs; its output is captured to a per-session log.
//
// Because the PTY is owned by this process, sessions live and die with the
// launcher — so the store is in-memory (no on-disk registry to go stale). The
// actual session is still driven from claude.ai/code; we only spawn + track.
package sessions

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	xpty "github.com/aymanbagabas/go-pty"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/shells"
)

const (
	logCap     = 256 * 1024 // keep the useful startup/connection output, cap growth
	tailBytes  = 16 * 1024
	promptWind = 4096 // rolling ANSI-stripped window scanned for startup gates

	// How long the startup gates stay armed. Whichever comes first: remote
	// control connecting, every known gate answered, this many bytes, or this
	// long. After that the scanner shuts off for good.
	gateWindow      = 90 * time.Second
	gateWindowBytes = 64 * 1024
)

type Session struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Dir        string `json:"dir"`
	Shell      string `json:"shell"` // shell id
	ShellLabel string `json:"shellLabel"`
	Model      string `json:"model"` // model id passed to `claude --model`
	ModelLabel string `json:"modelLabel"`
	PID        int    `json:"pid"`
	StartedAt  int64  `json:"startedAt"` // epoch ms
	LogFile    string `json:"logFile"`
	Status     string `json:"status"` // "running" | "stopped"

	// Set by the leader when aggregating the ring; empty on a node's own rows.
	Node      string `json:"node,omitempty"`
	NodeLabel string `json:"nodeLabel,omitempty"`
}

// On first launch, `claude` can block on interactive onboarding gates BEFORE it
// brings up the remote-control channel — e.g. the per-directory "Is this a
// project you trust?" prompt, or the "Claude in Chrome extension detected" one.
// Nobody can answer them (the only input path is remote control, which isn't up
// yet), so the session deadlocks and exits. Since the launcher operator
// deliberately picks the directory + model, we auto-answer each known gate once
// on their behalf. Match against ANSI-stripped output (the TUI wraps each word
// in its own colour escape, so phrases aren't contiguous in the raw stream).
//
// To add a gate: append an entry — Send is the key sequence to emit
// ("\r" = Enter / confirm the highlighted default, "\x1b" = Esc / cancel).
var (
	stripANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

	startupPrompts = []struct {
		ID    string
		Match *regexp.Regexp
		Send  string
	}{
		// Default highlighted: "Yes, I trust this folder" → Enter confirms trust.
		{"folder-trust", regexp.MustCompile(`(?i)trust this folder|is this a project you (created|trust)`), "\r"},
		// Default highlighted: "Yes, use my browser" → Enter turns browser tools on.
		{"chrome-extension", regexp.MustCompile(`(?i)chrome extension detected|use my browser`), "\r"},
		// Default highlighted: "Yes, try it" — but a headless PTY must NOT switch to
		// the fullscreen / alternate-screen renderer: it repaints over the captured
		// log and can stall the remote-control handshake. Esc = "Not now", keeping
		// the plain line-based renderer the launcher relies on.
		{"fullscreen-renderer", regexp.MustCompile(`(?i)try the new fullscreen renderer`), "\x1b"},
	}

	// Remote control is up: onboarding is definitively over, so the gates can
	// close even if some never fired.
	gatesDone = regexp.MustCompile(`(?i)remote.?control\s*is\s*active|/code/session_`)
)

type live struct {
	mu       sync.Mutex
	session  Session
	pty      xpty.Pty
	cmd      *xpty.Cmd
	log      *os.File
	logged   int  // bytes written so far (capped to keep logs bounded)
	exited   bool //
	head     []byte
	answered map[string]bool
	gateShut bool // startup window is over; stop scanning for gates

	// Closed by pump() when the PTY is drained, so reap() can write the exit
	// line and close the log knowing nothing else will touch them.
	pumped chan struct{}
}

type Manager struct {
	mu      sync.RWMutex
	items   map[string]*live
	cfg     config.Config
	catalog *catalog.Store
	shells  *shells.Registry
	logDir  string
}

func NewManager(cfg config.Config, store *catalog.Store, reg *shells.Registry) *Manager {
	return &Manager{
		items:   map[string]*live{},
		cfg:     cfg,
		catalog: store,
		shells:  reg,
		logDir:  filepath.Join(cfg.DataDir, "logs"),
	}
}

func (m *Manager) List() []Session {
	// Collect the pointers under m.mu, snapshot after releasing it: taking
	// l.mu while holding m.mu would let one slow session block every caller.
	m.mu.RLock()
	items := make([]*live, 0, len(m.items))
	for _, l := range m.items {
		items = append(items, l)
	}
	m.mu.RUnlock()

	out := make([]Session, 0, len(items))
	for _, l := range items {
		out = append(out, l.snapshot())
	}

	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

func (l *live) snapshot() Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.session
	s.Status = "running"
	if l.exited {
		s.Status = "stopped"
	}
	return s
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9 _-]`)

// sanitizeName keeps names to a safe charset so they embed in a shell command
// without escaping tricks.
func sanitizeName(raw string) string {
	s := strings.TrimSpace(unsafeName.ReplaceAllString(raw, ""))
	if len(s) > 60 {
		s = s[:60]
	}
	return strings.TrimSpace(s)
}

func defaultName(dir string) string {
	base := filepath.Base(strings.TrimRight(dir, `/\`))
	if n := sanitizeName(base); n != "" {
		return n
	}
	return "session"
}

func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b)
}

// Variables that identify a Claude Code session. If the launcher itself was
// started from inside one (a terminal running `claude`, say), the whole set is
// in its environment — and a session spawned with it inherited would believe it
// is a CHILD of that session: transcript saving silently turns off, and it
// reports the parent's session id. Strip them so every spawned session is its
// own top-level session.
var inheritedMarkers = []string{
	"CLAUDECODE",
	"CLAUDE_PID",
	"CLAUDE_EFFORT",
}

// …plus everything under this prefix (CLAUDE_CODE_CHILD_SESSION,
// CLAUDE_CODE_SESSION_ID, CLAUDE_CODE_BRIDGE_SESSION_ID, …).
const markerPrefix = "CLAUDE_CODE_"

var warnMarkers sync.Once

// sessionEnv is the launcher's environment minus those markers. It returns the
// names it dropped, which are noted in the session log — silently changing a
// session's environment would be worse than the leak.
func sessionEnv() (env []string, dropped []string) {
	defer func() {
		if len(dropped) > 0 {
			warnMarkers.Do(func() {
				log.Printf("this launcher runs inside a Claude Code session; stripping %s from spawned sessions "+
					"(start it from a plain shell to avoid this)", strings.Join(dropped, " "))
			})
		}
	}()

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, markerPrefix) || slices.Contains(inheritedMarkers, name) {
			dropped = append(dropped, name)
			continue
		}
		env = append(env, kv)
	}
	return env, dropped
}

// uptime renders how long a session ran, for the lifecycle log lines.
func uptime(startedAt int64) string {
	return time.Since(time.UnixMilli(startedAt)).Round(time.Second).String()
}

type SpawnOpts struct {
	Dir   string `json:"dir"`
	Shell string `json:"shell"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

func (m *Manager) Spawn(opts SpawnOpts) (Session, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return Session{}, fmt.Errorf("directory does not exist: (empty)")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Session{}, fmt.Errorf("directory does not exist: %s", dir)
	}

	sh, ok := m.shells.Get(strings.TrimSpace(opts.Shell))
	if !ok {
		name := strings.TrimSpace(opts.Shell)
		if name == "" {
			name = "(none)"
		}
		return Session{}, fmt.Errorf("unknown or unavailable shell: %s", name)
	}

	model, err := m.catalog.ResolveModel(opts.Model)
	if err != nil {
		return Session{}, err
	}

	id := newID()
	name := sanitizeName(opts.Name)
	if name == "" {
		name = defaultName(dir)
	}

	// Only quote the name when needed — keeps cmd.exe happy for the common
	// no-space case. The model id is ALWAYS quoted: ids like
	// `claude-opus-4-8[1m]` contain brackets that bash/PowerShell would
	// otherwise treat as globs.
	q := ""
	if strings.Contains(name, " ") {
		q = sh.Quote
	}
	command := fmt.Sprintf("%s --model %s%s%s --remote-control %s%s%s",
		m.cfg.ClaudeBin, sh.Quote, model.ID, sh.Quote, q, name, q)

	if err := os.MkdirAll(m.logDir, 0o755); err != nil {
		return Session{}, fmt.Errorf("cannot create log dir: %w", err)
	}
	logFile := filepath.Join(m.logDir, id+".log")
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Session{}, fmt.Errorf("cannot open log: %w", err)
	}

	p, err := xpty.New()
	if err != nil {
		lf.Close()
		return Session{}, fmt.Errorf("cannot open pty: %w", err)
	}
	if err := p.Resize(120, 30); err != nil {
		// Non-fatal: a default-sized PTY still works.
		fmt.Fprintf(lf, "[claude-launcher: pty resize failed: %v]\n", err)
	}

	cmd := p.Command(sh.Bin, sh.Argv(command)...)
	cmd.Dir = dir
	env, dropped := sessionEnv()
	cmd.Env = env
	if len(dropped) > 0 {
		fmt.Fprintf(lf, "[claude-launcher: dropped inherited Claude Code markers: %s]\n", strings.Join(dropped, " "))
	}
	if err := cmd.Start(); err != nil {
		p.Close()
		lf.Close()
		return Session{}, fmt.Errorf("cannot start %s: %w", sh.Bin, err)
	}

	l := &live{
		session: Session{
			ID:         id,
			Name:       name,
			Dir:        dir,
			Shell:      sh.ID,
			ShellLabel: sh.Label,
			Model:      model.ID,
			ModelLabel: model.Label,
			PID:        cmd.Process.Pid,
			StartedAt:  time.Now().UnixMilli(),
			LogFile:    logFile,
		},
		pty:      p,
		cmd:      cmd,
		log:      lf,
		answered: map[string]bool{},
		pumped:   make(chan struct{}),
	}

	m.mu.Lock()
	m.items[id] = l
	m.mu.Unlock()

	log.Printf("session %s %q started · %s · %s · %s · pid %d",
		id, name, sh.ID, model.ID, dir, cmd.Process.Pid)

	go l.pump()
	go l.reap()

	return l.snapshot(), nil
}

// pump drains the PTY: tees output to the log (capped) and auto-answers the
// known interactive startup gates.
func (l *live) pump() {
	buf := make([]byte, 8192)
	defer close(l.pumped)
	for {
		n, err := l.pty.Read(buf)
		if n > 0 {
			l.onData(buf[:n])
		}
		if err != nil {
			return // EOF / EIO once the child is gone; reap() records the exit
		}
	}
}

// onData handles one chunk of PTY output. All I/O happens OUTSIDE l.mu: the log
// can sit on a slow or full disk, and blocking there while holding the lock
// would wedge Manager.List() and, through it, every Spawn/Remove/Shutdown.
// Ordering is safe without the lock because pump() is the only writer to the
// log while a session runs, and reap() waits for pump() before touching it.
func (l *live) onData(data []byte) {
	l.mu.Lock()
	write := l.logged < logCap
	if write {
		l.logged += len(data)
	}
	capped := write && l.logged >= logCap
	gates := l.scanGatesLocked(data)
	l.mu.Unlock()

	if write {
		l.log.Write(data)
	}
	if capped {
		l.log.WriteString("\n…[log capped]…\n")
	}
	for _, p := range gates {
		// One keystroke; it cannot realistically fill the PTY's input buffer,
		// which matters because pump() (this goroutine) is its only reader.
		if _, err := l.pty.Write([]byte(p.send)); err == nil {
			fmt.Fprintf(l.log, "\n[claude-launcher: auto-answered %s prompt]\n", p.id)
			log.Printf("session %s auto-answered the %s prompt", l.session.ID, p.id)
		}
	}
}

type gateHit struct{ id, send string }

// scanGatesLocked matches the startup gates against a rolling ANSI-stripped
// window and returns the ones to answer. Caller holds l.mu and does the writing.
//
// The window is only scanned during STARTUP — see gateWindow. Once it closes we
// stop looking, which is the whole point: these patterns are ordinary English
// and a long-lived session will eventually print something that matches (a
// session reading this repo's own README would), and answering then would put a
// stray Enter on the stdin of a live session, confirming whatever prompt the TUI
// happened to be showing.
func (l *live) scanGatesLocked(data []byte) []gateHit {
	if l.gateShut {
		return nil
	}
	switch {
	case len(l.answered) >= len(startupPrompts):
		// Every known gate answered — nothing left to wait for.
	case l.logged > gateWindowBytes:
		// Past any plausible startup banner.
	case time.Since(time.UnixMilli(l.session.StartedAt)) > gateWindow:
		// Took too long to connect; whatever is happening isn't onboarding.
	default:
		l.head = append(l.head, stripANSI.ReplaceAll(data, nil)...)
		if len(l.head) > promptWind {
			l.head = l.head[len(l.head)-promptWind:]
		}
		var hits []gateHit
		for _, p := range startupPrompts {
			if !l.answered[p.ID] && p.Match.Match(l.head) {
				l.answered[p.ID] = true
				hits = append(hits, gateHit{p.ID, p.Send})
			}
		}
		// Remote control coming up is the positive signal that startup is over.
		if gatesDone.Match(l.head) {
			l.shutGatesLocked()
		}
		return hits
	}

	l.shutGatesLocked()
	return nil
}

func (l *live) shutGatesLocked() {
	l.gateShut = true
	l.head = nil // the scan window is dead weight from here on
}

// reap waits for the shell to exit and closes out the session's resources.
//
// Order matters. The PTY master can still hold unread bytes after the child is
// gone, and pump() runs on its own goroutine: closing the log first would drop
// the last output before an exit — exactly the lines you need when a launch
// fails, silently, since a write to a closed *os.File just returns an error
// nobody reads. So: close the PTY (which ends pump's blocking read), wait for
// pump to finish, and only then write the exit line and close the log.
func (l *live) reap() {
	err := l.cmd.Wait()

	l.mu.Lock()
	l.exited = true
	code := 0
	if l.cmd.ProcessState != nil {
		code = l.cmd.ProcessState.ExitCode()
	} else if err != nil {
		code = -1
	}
	l.mu.Unlock()

	l.pty.Close()
	<-l.pumped

	fmt.Fprintf(l.log, "\n[exited code=%d]\n", code)
	log.Printf("session %s %q exited code=%d after %s",
		l.session.ID, l.session.Name, code, uptime(l.session.StartedAt))
	l.log.Close()
}

// Remove kills the session (if running) and drops it from the store.
func (m *Manager) Remove(id string) bool {
	m.mu.Lock()
	l, ok := m.items[id]
	if ok {
		delete(m.items, id)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}

	l.mu.Lock()
	exited := l.exited
	l.mu.Unlock()
	if !exited && l.cmd.Process != nil {
		// Kill the whole process group: the PTY runs a login shell which in
		// turn exec'd `claude`, and killing only the shell would orphan it.
		log.Printf("session %s %q killed after %s", id, l.session.Name, uptime(l.session.StartedAt))
		killGroup(l.cmd.Process)
	} else {
		log.Printf("session %s %q removed (already stopped)", id, l.session.Name)
	}
	return true
}

// TailLog returns the last bytes of a session's captured output.
func (m *Manager) TailLog(id string) (string, bool) {
	m.mu.RLock()
	l, ok := m.items[id]
	m.mu.RUnlock()
	if !ok {
		return "", false
	}

	b, err := os.ReadFile(l.session.LogFile)
	if err != nil {
		return "", true
	}
	if len(b) > tailBytes {
		b = b[len(b)-tailBytes:]
	}
	return string(b), true
}

// Shutdown kills every live session. Called on SIGINT/SIGTERM so a restart
// doesn't leave orphaned `claude` processes attached to dead PTYs.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	items := make([]*live, 0, len(m.items))
	for _, l := range m.items {
		items = append(items, l)
	}
	m.items = map[string]*live{}
	m.mu.Unlock()

	for _, l := range items {
		l.mu.Lock()
		exited := l.exited
		l.mu.Unlock()
		if !exited && l.cmd.Process != nil {
			killGroup(l.cmd.Process)
		}
	}
}
