//go:build windows

package sessions

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// setGroup gives cmd its own process group. killGroup uses `taskkill /T` to
// walk the child tree by pid, which works regardless — but a new group keeps
// a Ctrl-Break/console signal aimed at the launcher from reaching the turn's
// `claude` process. NOTE: not verified on a real Windows host (developed and
// tested on macOS); the taskkill path that killGroup already relies on is
// unchanged by this.
func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killGroup terminates the shell and everything it spawned. Windows has no
// process groups to signal, so use taskkill /T to walk the child tree (the
// shell → `claude`), falling back to killing the shell alone.
func killGroup(p *os.Process) {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	if err := cmd.Run(); err != nil {
		_ = p.Kill()
	}
}
