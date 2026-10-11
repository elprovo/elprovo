package agent

import (
	"bytes"
	"context"
	"io"
)

// stepMachine runs commands on one machine for one step of a job and sends
// their output to the job log.
type stepMachine struct {
	machines Machines
	name     string
	rep      *reporter
	step     string
}

// Run runs a command and returns its standard output. Standard error goes
// to the job log, and its end into the error.
func (m stepMachine) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	stderr := m.rep.writer(m.step, "stderr")
	tail := &tailBuffer{max: 1500}
	err := m.machines.Run(ctx, m.name, stdin, &stdout, io.MultiWriter(stderr, tail), args...)
	_ = stderr.Close()
	return stdout.Bytes(), withTail(err, tail)
}

// Stream runs a command and sends all its output to the job log, with the
// end of the output in the error.
func (m stepMachine) Stream(ctx context.Context, stdin io.Reader, args ...string) error {
	stdout := m.rep.writer(m.step, "stdout")
	stderr := m.rep.writer(m.step, "stderr")
	tail := &tailBuffer{max: 1500}
	err := m.machines.Run(ctx, m.name, stdin, io.MultiWriter(stdout, tail), io.MultiWriter(stderr, tail), args...)
	_ = stdout.Close()
	_ = stderr.Close()
	return withTail(err, tail)
}

// CopyIn copies a file from the agent host into the machine.
func (m stepMachine) CopyIn(ctx context.Context, localPath, remotePath string) error {
	return m.machines.CopyIn(ctx, m.name, localPath, remotePath)
}
