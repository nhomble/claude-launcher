//go:build !windows

package sessions

import (
	"os"
	"syscall"
	"time"
)

// killGroup terminates the PTY's whole process group. go-pty starts the shell
// with Setsid, so the shell is a session/group leader and its pgid equals its
// pid — signalling -pid reaches `claude` too, not just the shell.
//
// SIGTERM first so claude can shut its remote-control channel down cleanly,
// then SIGKILL shortly after for anything that ignored it.
func killGroup(p *os.Process) {
	pgid := -p.Pid
	if syscall.Kill(pgid, syscall.SIGTERM) != nil {
		// Not a group leader (or already reaped) — fall back to the process.
		_ = p.Signal(syscall.SIGTERM)
	}
	go func() {
		time.Sleep(2 * time.Second)
		if syscall.Kill(pgid, syscall.SIGKILL) != nil {
			_ = p.Kill()
		}
	}()
}
