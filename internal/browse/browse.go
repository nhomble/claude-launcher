// Package browse is a minimal server-side directory browser for the UI's
// folder picker. It lists only SUBDIRECTORIES (you launch a session in a
// directory), plus quick-jump anchors (home + drive roots on Windows).
//
// Same trust posture as the rest of the app: tailnet-only, no auth — and
// strictly less powerful than spawning a shell, which this app already does.
package browse

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Result struct {
	Path    string   `json:"path"`             // current absolute directory
	Parent  string   `json:"parent"`           // parent dir, or "" at a root
	Home    string   `json:"home"`             //
	Drives  []string `json:"drives,omitempty"` // Windows drive roots (C:\, D:\, …)
	Entries []Entry  `json:"entries"`          // immediate subdirectories
	Error   string   `json:"error,omitempty"`
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return string(filepath.Separator)
}

func listDrives() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var out []string
	for c := 'A'; c <= 'Z'; c++ {
		d := fmt.Sprintf("%c:\\", c)
		if _, err := os.Stat(d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// Browse lists the subdirectories of input, falling back to its parent and then
// to $HOME when the path is missing or unreadable.
func Browse(input string) Result {
	h := home()
	path := strings.TrimSpace(input)
	if path == "" {
		path = h
	} else {
		path, _ = filepath.Abs(path)
	}

	res := Result{Home: h, Drives: listDrives(), Entries: []Entry{}}

	st, err := os.Stat(path)
	switch {
	case err != nil:
		res.Error = "cannot access: " + path
		path = h
	case !st.IsDir():
		path = filepath.Dir(path)
	}
	res.Path = path

	dirents, err := os.ReadDir(path)
	if err != nil {
		res.Error = "cannot read: " + path
	}
	for _, d := range dirents {
		if !d.IsDir() {
			// Follow symlinks: a symlinked project dir is still launchable.
			if d.Type()&os.ModeSymlink == 0 {
				continue
			}
			if st, err := os.Stat(filepath.Join(path, d.Name())); err != nil || !st.IsDir() {
				continue
			}
		}
		res.Entries = append(res.Entries, Entry{Name: d.Name(), Path: filepath.Join(path, d.Name())})
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		return strings.ToLower(res.Entries[i].Name) < strings.ToLower(res.Entries[j].Name)
	})

	if parent := filepath.Dir(path); parent != path {
		res.Parent = parent
	}
	return res
}

// MakeDir creates one new subdirectory name directly under parent and returns
// its absolute path. A single path segment only — no separators, no traversal,
// no Windows-hostile chars — so this can never escape the chosen parent. Same
// trust posture as Browse(), and weaker than the shell this app spawns.
func MakeDir(parent, name string) (string, error) {
	base := strings.TrimSpace(parent)
	if base == "" {
		return "", fmt.Errorf("parent directory does not exist: (empty)")
	}
	base, _ = filepath.Abs(base)
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return "", fmt.Errorf("parent directory does not exist: %s", base)
	}

	raw := strings.TrimSpace(name)
	// Reject empties, dot-names, path separators/drive colons, and a trailing
	// dot or space (illegal/invisible on Windows).
	if raw == "" || raw == "." || raw == ".." ||
		strings.ContainsAny(raw, `\/:*?"<>|`) ||
		strings.HasSuffix(raw, ".") || strings.HasSuffix(raw, " ") {
		if raw == "" {
			raw = "(empty)"
		}
		return "", fmt.Errorf("invalid folder name: %s", raw)
	}

	target := filepath.Join(base, raw)
	// Defense in depth: the result must sit DIRECTLY inside parent.
	if filepath.Dir(target) != base {
		return "", fmt.Errorf("folder name must be a single path segment")
	}
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("already exists: %s", target)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return "", err
	}
	return target, nil
}
