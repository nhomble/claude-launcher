// Package catalog owns the two lists the launcher is configured with — the
// models offered in the dropdown, and the shells it knows how to run a session
// through.
//
// Both live in one config.yaml. A file on disk REPLACES the built-in defaults
// wholesale (so you can drop entries you never use), and is re-read whenever it
// changes: restarting the launcher would kill every live session, so editing
// the catalog must never require one. A file that fails to parse or validate is
// ignored — the last good catalog stays in force and the error is logged.
package catalog

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed defaults.yaml
var defaultsYAML []byte

// Placeholder replaced by the `claude …` command line inside a shell's args.
const CommandPlaceholder = "{{command}}"

type Model struct {
	ID      string `yaml:"id" json:"id"`
	Label   string `yaml:"label" json:"label"`
	Default bool   `yaml:"default,omitempty" json:"default,omitempty"`
}

type ShellSpec struct {
	ID         string   `yaml:"id" json:"id"`
	Label      string   `yaml:"label" json:"label"`
	OS         string   `yaml:"os" json:"os"` // posix | windows
	Candidates []string `yaml:"candidates" json:"candidates"`
	Quote      string   `yaml:"quote" json:"quote"`
	Args       []string `yaml:"args" json:"args"`
}

// Argv builds the argv that runs command through this shell.
func (s ShellSpec) Argv(command string) []string {
	out := make([]string, len(s.Args))
	for i, a := range s.Args {
		if a == CommandPlaceholder {
			out[i] = command
			continue
		}
		out[i] = a
	}
	return out
}

type file struct {
	Models []Model     `yaml:"models"`
	Shells []ShellSpec `yaml:"shells"`
}

// Store holds the current catalog and reloads it when config.yaml changes.
// The zero value is not usable — build one with New.
type Store struct {
	path string // "" when running on the built-in defaults

	mu    sync.RWMutex
	cur   file
	gen   uint64 // bumped on every successful (re)load; caches key off this
	stamp string // mtime+size of the loaded file, "" for defaults
}

// New builds a store. path may be empty, in which case only the built-in
// defaults are used. An unreadable or invalid file is a startup error — a
// launcher silently running someone else's model list is worse than not
// starting.
func New(path string) (*Store, error) {
	s := &Store{path: path}

	def, err := parse(defaultsYAML)
	if err != nil {
		return nil, fmt.Errorf("built-in defaults are invalid: %w", err) // programmer error
	}
	s.cur, s.gen = def, 1

	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Nothing there yet — run on the defaults and keep watching, so
		// dropping a config.yaml in later takes effect without a restart.
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	loaded, err := parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.cur, s.gen, s.stamp = loaded, 2, stampOf(path)
	return s, nil
}

// Path is the config file in force, or "" when running on the defaults.
func (s *Store) Path() string { return s.path }

// Gen identifies the current catalog. It changes on every reload, so callers
// caching derived state (shell detection, say) can invalidate on it.
func (s *Store) Gen() uint64 {
	s.reloadIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gen
}

func (s *Store) Models() []Model {
	s.reloadIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Model(nil), s.cur.Models...)
}

// Shells returns only the specs that apply to this host's platform, in file
// order — the first is the UI's default.
func (s *Store) Shells() []ShellSpec {
	s.reloadIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()

	want := "posix"
	if runtime.GOOS == "windows" {
		want = "windows"
	}
	var out []ShellSpec
	for _, sh := range s.cur.Shells {
		if sh.OS == want {
			out = append(out, sh)
		}
	}
	return out
}

func (s *Store) DefaultModel() Model {
	ms := s.Models()
	for _, m := range ms {
		if m.Default {
			return m
		}
	}
	if len(ms) > 0 {
		return ms[0]
	}
	return Model{}
}

// ResolveModel maps a requested id to a configured model, or the default when
// the request is empty.
func (s *Store) ResolveModel(id string) (Model, error) {
	want := strings.TrimSpace(id)
	if want == "" {
		m := s.DefaultModel()
		if m.ID == "" {
			return Model{}, fmt.Errorf("no models configured")
		}
		return m, nil
	}
	for _, m := range s.Models() {
		if m.ID == want {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("unknown model: %s", want)
}

// Defaults is the built-in catalog as YAML — what `--dump-config` prints, so a
// user starts from a complete, commented file.
func Defaults() []byte { return defaultsYAML }

// ── loading ──────────────────────────────────────────────────────────────────

// reloadIfChanged re-reads the file when its mtime or size moved. Cheap enough
// to do per read: one stat, and a parse only when the file actually changed.
func (s *Store) reloadIfChanged() {
	if s.path == "" {
		return
	}
	now := stampOf(s.path)

	s.mu.RLock()
	unchanged := now == s.stamp
	s.mu.RUnlock()
	if unchanged {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if now == s.stamp { // another goroutine got there first
		return
	}
	// Record the stamp even on failure, so a broken file is reported once
	// rather than on every request until it is fixed.
	s.stamp = now

	if now == "" {
		log.Printf("catalog: %s disappeared; keeping the last good config", s.path)
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		log.Printf("catalog: cannot re-read %s (%v); keeping the last good config", s.path, err)
		return
	}
	loaded, err := parse(b)
	if err != nil {
		log.Printf("catalog: %s is invalid (%v); keeping the last good config", s.path, err)
		return
	}
	s.cur = loaded
	s.gen++
	log.Printf("catalog: reloaded %s (%d models, %d shells)", s.path, len(loaded.Models), len(loaded.Shells))
}

// stampOf is a cheap change token: mtime+size. "" when the file is gone.
func stampOf(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

func parse(b []byte) (file, error) {
	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a typo'd key is a mistake, not a silent no-op
	if err := dec.Decode(&f); err != nil {
		return file{}, err
	}
	if err := validate(&f); err != nil {
		return file{}, err
	}
	return f, nil
}

func validate(f *file) error {
	if len(f.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	seen := map[string]bool{}
	defaults := 0
	for i := range f.Models {
		m := &f.Models[i]
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" {
			return fmt.Errorf("models[%d]: id is required", i)
		}
		if seen[m.ID] {
			return fmt.Errorf("models[%d]: duplicate id %q", i, m.ID)
		}
		seen[m.ID] = true
		if m.Label == "" {
			m.Label = m.ID
		}
		if m.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("only one model may set default: true (%d do)", defaults)
	}

	if len(f.Shells) == 0 {
		return fmt.Errorf("no shells configured")
	}
	seen = map[string]bool{}
	for i := range f.Shells {
		sh := &f.Shells[i]
		sh.ID = strings.TrimSpace(sh.ID)
		if sh.ID == "" {
			return fmt.Errorf("shells[%d]: id is required", i)
		}
		if seen[sh.ID] {
			return fmt.Errorf("shells[%d]: duplicate id %q", i, sh.ID)
		}
		seen[sh.ID] = true
		if sh.Label == "" {
			sh.Label = sh.ID
		}
		switch sh.OS {
		case "posix", "windows":
		case "":
			return fmt.Errorf("shells[%s]: os is required (posix or windows)", sh.ID)
		default:
			return fmt.Errorf("shells[%s]: unknown os %q (want posix or windows)", sh.ID, sh.OS)
		}
		if len(sh.Candidates) == 0 {
			return fmt.Errorf("shells[%s]: at least one candidate is required", sh.ID)
		}
		if sh.Quote == "" {
			sh.Quote = "'"
		}
		n := 0
		for _, a := range sh.Args {
			if a == CommandPlaceholder {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("shells[%s]: args must contain exactly one %s element (found %d)", sh.ID, CommandPlaceholder, n)
		}
	}
	return nil
}

// FindConfig resolves which config.yaml the launcher watches: an explicit path
// (which must exist), else config.yaml in the working directory, else one next
// to the binary. When neither exists it still returns the working-directory
// path — the store runs on the defaults until a file shows up there.
func FindConfig(explicit string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file not found: %s", explicit)
		}
		return abs(explicit), nil
	}
	candidates := []string{abs("config.yaml")}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.yaml"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return candidates[0], nil
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
