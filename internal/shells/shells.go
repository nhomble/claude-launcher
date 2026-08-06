// Package shells does cross-platform shell detection.
//
// Each session is launched THROUGH a shell so the shell sources the user's
// profile/rc (PATH, env, nvm, aliases) before exec'ing `claude` — that's the
// whole point of letting the user pick a shell.
//
// Args(command) returns the argv that runs command via this shell with its
// config loaded:
//   - PowerShell: the profile loads by default (we do NOT pass -NoProfile).
//   - POSIX shells: `-l -i` so both the login profile AND the rc are sourced.
//
// Quote is the character used to wrap a value (the session name, the model id)
// inside the command string for this shell.
package shells

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

type Shell struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Bin   string `json:"-"`
	Quote string `json:"-"`

	args func(command string) []string
}

// Args builds the argv running command through this shell.
func (s Shell) Args(command string) []string { return s.args(command) }

type spec struct {
	id, label string
	quote     string
	// candidates are tried in order: a path is checked for existence, a bare
	// name is resolved on PATH.
	candidates []string
	args       func(command string) []string
}

func pwshArgs(cmd string) []string  { return []string{"-NoLogo", "-Command", cmd} }
func cmdArgs(cmd string) []string   { return []string{"/c", cmd} }
func loginArgs(cmd string) []string { return []string{"-l", "-i", "-c", cmd} }
func plainArgs(cmd string) []string { return []string{"-c", cmd} }

var windowsShells = []spec{
	{id: "powershell", label: "Windows PowerShell", quote: "'", candidates: []string{"powershell.exe", "powershell"}, args: pwshArgs},
	{id: "pwsh", label: "PowerShell 7 (pwsh)", quote: "'", candidates: []string{"pwsh.exe", "pwsh"}, args: pwshArgs},
	{id: "cmd", label: "Command Prompt", quote: `"`, candidates: []string{"cmd.exe"}, args: cmdArgs},
	{id: "git-bash", label: "Git Bash", quote: "'", candidates: []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		"bash.exe",
	}, args: loginArgs},
}

var posixShells = []spec{
	{id: "zsh", label: "zsh", quote: "'", candidates: []string{"zsh"}, args: loginArgs},
	{id: "bash", label: "bash", quote: "'", candidates: []string{"bash"}, args: loginArgs},
	{id: "fish", label: "fish", quote: "'", candidates: []string{"fish"}, args: loginArgs},
	{id: "sh", label: "sh", quote: "'", candidates: []string{"sh"}, args: plainArgs},
}

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

var (
	once  sync.Once
	found []Shell
)

// Available lists the shells actually installed on this host, in preference
// order. Detection runs once per process.
func Available() []Shell {
	once.Do(func() {
		specs := posixShells
		if runtime.GOOS == "windows" {
			specs = windowsShells
		}
		for _, s := range specs {
			if bin := resolveBin(s.candidates); bin != "" {
				found = append(found, Shell{ID: s.id, Label: s.label, Bin: bin, Quote: s.quote, args: s.args})
			}
		}
	})
	return found
}

// Get resolves a shell id to a detected shell.
func Get(id string) (Shell, bool) {
	for _, s := range Available() {
		if s.ID == id {
			return s, true
		}
	}
	return Shell{}, false
}

// IDs is a convenience for logging/healthz.
func IDs() []string {
	out := make([]string, 0, len(Available()))
	for _, s := range Available() {
		out = append(out, s.ID)
	}
	return out
}
