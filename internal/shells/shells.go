// Package shells resolves the configured shell specs (see package catalog)
// against what is actually installed on this host.
//
// Each session is launched THROUGH a shell so the shell sources the user's
// profile/rc (PATH, env, nvm, aliases) before exec'ing `claude` — that's the
// whole point of letting the user pick a shell.
package shells

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/nhomble/claude-launcher/internal/catalog"
)

// Shell is a spec that resolved to a real binary on this machine.
type Shell struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Bin   string `json:"-"`
	Quote string `json:"-"`

	spec catalog.ShellSpec
}

// Argv builds the argv running command through this shell.
func (s Shell) Argv(command string) []string { return s.spec.Argv(command) }

// Registry caches detection per catalog generation: probing PATH on every
// request would be wasteful, but the cache must drop when config.yaml changes.
type Registry struct {
	catalog *catalog.Store

	mu    sync.Mutex
	gen   uint64
	found []Shell
}

func NewRegistry(store *catalog.Store) *Registry { return &Registry{catalog: store} }

// Available lists the shells installed on this host, in configured order.
func (r *Registry) Available() []Shell {
	gen := r.catalog.Gen()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen == gen && r.found != nil {
		return r.found
	}

	found := []Shell{}
	for _, spec := range r.catalog.Shells() {
		if bin := resolveBin(spec.Candidates); bin != "" {
			found = append(found, Shell{ID: spec.ID, Label: spec.Label, Bin: bin, Quote: spec.Quote, spec: spec})
		}
	}
	r.gen, r.found = gen, found
	return found
}

// Get resolves a shell id to a detected shell.
func (r *Registry) Get(id string) (Shell, bool) {
	for _, s := range r.Available() {
		if s.ID == id {
			return s, true
		}
	}
	return Shell{}, false
}

// IDs is a convenience for logging and /healthz.
func (r *Registry) IDs() []string {
	av := r.Available()
	out := make([]string, 0, len(av))
	for _, s := range av {
		out = append(out, s.ID)
	}
	return out
}

// resolveBin returns the first candidate that exists: a path is checked
// directly, a bare name is looked up on PATH.
func resolveBin(candidates []string) string {
	for _, c := range candidates {
		if strings.ContainsAny(c, `/\`) {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}
