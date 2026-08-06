// Package recents keeps a persistent history of directories you've launched
// sessions in. It lives server-side (not in browser localStorage) so the list
// is shared across every device reaching this host — your laptop and your phone
// see the same recents. Most-recent-first, de-duplicated, capped.
package recents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const cap_ = 15

type Store struct {
	mu   sync.Mutex
	file string
	dir  string
}

func New(dataDir string) *Store {
	return &Store{dir: dataDir, file: filepath.Join(dataDir, "recents.json")}
}

func (s *Store) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

func (s *Store) read() []string {
	b, err := os.ReadFile(s.file)
	if err != nil {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return []string{}
	}
	return out
}

// Add moves dir to the front of the list. Best-effort: a write failure is not
// worth failing a launch over.
func (s *Store) Add(dir string) {
	d := strings.TrimSpace(dir)
	if d == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	next := []string{d}
	for _, x := range s.read() {
		if x != d {
			next = append(next, x)
		}
	}
	if len(next) > cap_ {
		next = next[:cap_]
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.file, b, 0o644)
}
