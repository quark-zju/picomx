package archive

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const gitOperationTimeout = 30 * time.Second

var ErrGitTimeout = errors.New("Git operation timed out")

// Each Git command has its own process group. Kill the group on cancellation,
// including pack-objects/repack children, before releasing repository locks.
func killGit(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

func watchGit(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, operation func() error) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	stop, finished := make(chan struct{}), make(chan struct{})
	var interruption error
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			interruption = ctx.Err()
			killGit(cmd)
		case <-timer.C:
			interruption = ErrGitTimeout
			killGit(cmd)
		case <-stop:
		}
	}()
	err := operation()
	close(stop)
	<-finished
	return errors.Join(interruption, err)
}

func (process *gitProcess) watch(operation func() error) error {
	timeout := process.timeout
	if timeout <= 0 {
		timeout = gitOperationTimeout
	}
	return watchGit(context.Background(), process.cmd, timeout, operation)
}
