//go:build unix

package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a command's process group has between SIGTERM and
// SIGKILL.
const killGrace = 10 * time.Second

// commandResult is how one command ended.
type commandResult struct {
	// exitCode is the command's exit status, or -1 when it did not exit by
	// itself.
	exitCode    int
	timedOut    bool
	interrupted bool
}

func (r commandResult) ok() bool { return r.exitCode == 0 && !r.timedOut && !r.interrupted }

// describe says how a command that did not succeed ended, for a reason in
// the report.
func (r commandResult) describe(name string, timeout time.Duration) string {
	switch {
	case r.interrupted:
		return name + " was interrupted"
	case r.timedOut:
		return fmt.Sprintf("%s did not end within %s", name, formatDuration(timeout))
	case r.exitCode < 0:
		return name + " was ended by a signal"
	default:
		return fmt.Sprintf("%s exited with status %d", name, r.exitCode)
	}
}

// runCommand runs command with /bin/sh -c in dir, in its own process group,
// and appends its output to logPath. When the command takes longer than
// timeout, or ctx ends first, it sends SIGTERM to the process group and
// SIGKILL after killGrace.
func runCommand(ctx context.Context, command, dir string, env []string, logPath string, timeout time.Duration) (commandResult, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return commandResult{}, err
	}
	defer log.Close()

	// The log is an *os.File, so the command writes to it directly. Wait then
	// returns when the shell exits, even when the command leaves a process
	// running that keeps the file open, as a server started by start does.
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return commandResult{}, fmt.Errorf("start /bin/sh: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var result commandResult
	select {
	case err := <-done:
		return exited(err)
	case <-timer.C:
		result.timedOut = true
	case <-ctx.Done():
		result.interrupted = true
	}

	result.exitCode = -1
	endGroup(-cmd.Process.Pid, done)
	return result, nil
}

// endGroup sends SIGTERM to a process group, and SIGKILL after killGrace when
// any process in it still runs, even after the shell itself has exited.
// shellDone receives once when the shell exits.
func endGroup(group int, shellDone <-chan error) {
	_ = syscall.Kill(group, syscall.SIGTERM)
	grace := time.NewTimer(killGrace)
	defer grace.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-shellDone:
			shellDone = nil
		case <-poll.C:
		case <-grace.C:
			_ = syscall.Kill(group, syscall.SIGKILL)
			if shellDone != nil {
				<-shellDone
			}
			return
		}
		// Signal 0 checks whether any process in the group still exists.
		if shellDone == nil && errors.Is(syscall.Kill(group, 0), syscall.ESRCH) {
			return
		}
	}
}

func exited(err error) (commandResult, error) {
	if err == nil {
		return commandResult{exitCode: 0}, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return commandResult{}, err
	}
	// ExitCode is -1 when a signal ended the shell.
	return commandResult{exitCode: exitErr.ExitCode()}, nil
}

func formatDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}
