package runner

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// exitInfo is what a child reported when it finished.
//
// Code is -1 when there is no exit code to report: either the process was
// killed by a signal (Signaled) or the wait itself failed. The previous
// implementation saw `code === null` in exactly those cases, and the record
// keeps that distinction because "killed" is not "exited with a status".
type exitInfo struct {
	Code     int
	Signaled bool
}

// childProcess is one spawned cloudflared. The supervisor talks to a child only
// through this interface, which is what lets the tests script a process instead
// of running a real binary.
type childProcess interface {
	PID() int
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait blocks until the child has exited. It is called exactly once.
	Wait() exitInfo
	// Terminate asks the child to exit (SIGTERM).
	Terminate() error
	// Kill ends the child without asking (SIGKILL).
	Kill() error
}

// spawnFunc starts a child. bin, args, env and dir mirror the previous
// child_process.spawn call: cwd PKGVAR, stdio ignore/pipe/pipe.
type spawnFunc func(bin string, args, env []string, dir string) (childProcess, error)

// spawnExec is the real spawner.
//
// The pipes are created here rather than through cmd.StdoutPipe so that Wait
// does not race the readers: exec only closes the descriptors it owns, and the
// parent's write ends are dropped immediately after Start, so the read ends see
// EOF the moment the child exits.
func spawnExec(bin string, args, env []string, dir string) (childProcess, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, err
	}
	// The child holds its own descriptors now.
	_ = outW.Close()
	_ = errW.Close()

	return &execChild{cmd: cmd, out: outR, err: errR}, nil
}

type execChild struct {
	cmd *exec.Cmd
	out *os.File
	err *os.File
}

func (c *execChild) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *execChild) Stdout() io.Reader { return c.out }
func (c *execChild) Stderr() io.Reader { return c.err }

func (c *execChild) Wait() exitInfo {
	err := c.cmd.Wait()
	if err == nil {
		return exitInfo{Code: 0}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// ExitCode reports -1 when the process was terminated by a signal.
		if code := exitErr.ExitCode(); code >= 0 {
			return exitInfo{Code: code}
		}
		return exitInfo{Code: -1, Signaled: true}
	}
	return exitInfo{Code: -1}
}

func (c *execChild) Terminate() error {
	if c.cmd.Process == nil {
		return nil
	}
	// On platforms without POSIX signals this returns an error; the SIGKILL
	// fallback in Stop is what actually ends the process there.
	return c.cmd.Process.Signal(syscall.SIGTERM)
}

func (c *execChild) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}
