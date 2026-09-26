//go:build windows

package sessions

import (
	"os"
	"os/exec"
	"strconv"
)

// killGroup terminates the shell and everything it spawned. Windows has no
// process groups to signal, so use taskkill /T to walk the child tree (the
// shell → `claude`), falling back to killing the shell alone.
func killGroup(p *os.Process) {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	if err := cmd.Run(); err != nil {
		_ = p.Kill()
	}
}
