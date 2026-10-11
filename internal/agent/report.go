package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/protocol"
)

// Log limits. Lines longer than maxLineBytes are cut; a reporter holds at
// most maxPendingLines unsent lines and drops the oldest beyond that.
const (
	maxLineBytes    = 4096
	maxRawLineBytes = 64 * 1024
	logBatchLines   = 200
	maxPendingLines = 5000
	logFlushEvery   = time.Second
)

// reporter sends one job's steps and output lines to the control plane.
// Sending is best effort: the job's result, not its log, decides what
// happens to the stagelet. Every line and step message passes through the
// redactor, so secret values never leave the agent.
type reporter struct {
	send       func(ctx context.Context, msgType string, payload any) error
	jobID      string
	leaseToken string
	log        *slog.Logger

	mu       sync.Mutex
	pending  []protocol.LogLine
	dropped  int
	secrets  []string
	replacer *strings.Replacer

	flushMu sync.Mutex
	full    chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func newReporter(send func(context.Context, string, any) error, job protocol.Job, log *slog.Logger) *reporter {
	return &reporter{
		send: send, jobID: job.ID, leaseToken: job.LeaseToken, log: log,
		full: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// start flushes lines every second, or sooner when a batch fills, until
// close.
func (r *reporter) start(ctx context.Context) {
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(logFlushEvery)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
			case <-r.full:
			}
			r.flush(ctx)
		}
	}()
}

// close stops the flusher and sends the remaining lines.
func (r *reporter) close(ctx context.Context) {
	close(r.stop)
	<-r.done
	r.flush(ctx)
}

// hideValues makes the reporter replace each value with [secret] in
// everything it sends. Output reaches the log line by line, so a value with
// line breaks is hidden line by line. Parts shorter than four characters
// are not hidden, because they would hide ordinary output.
func (r *reporter) hideValues(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		for _, part := range strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' }) {
			if len(strings.TrimSpace(part)) >= 4 {
				r.secrets = append(r.secrets, part)
			}
		}
	}
	pairs := make([]string, 0, 2*len(r.secrets))
	for _, s := range r.secrets {
		pairs = append(pairs, s, "[secret]")
	}
	r.replacer = strings.NewReplacer(pairs...)
}

func (r *reporter) redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.redactLocked(s)
}

func (r *reporter) redactLocked(s string) string {
	if r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}

// step reports name as started, runs fn, sends the lines it logged, and
// reports how it ended. A failed step's error names the step.
func (r *reporter) step(ctx context.Context, name string, fn func(context.Context) error) error {
	r.sendStep(ctx, name, protocol.StepStarted, "")
	err := fn(ctx)
	r.flush(ctx)
	if err != nil {
		err = fmt.Errorf("%s: %w", name, err)
		r.sendStep(ctx, name, protocol.StepFailed, err.Error())
		return err
	}
	r.sendStep(ctx, name, protocol.StepSucceeded, "")
	return nil
}

// skip reports that the job did not need step name.
func (r *reporter) skip(ctx context.Context, name, reason string) {
	r.sendStep(ctx, name, protocol.StepSkipped, reason)
}

func (r *reporter) sendStep(ctx context.Context, name, state, message string) {
	msg := protocol.JobStep{JobID: r.jobID, LeaseToken: r.leaseToken, Step: name, State: state, At: time.Now().UTC(), Message: r.redact(message)}
	if err := r.send(ctx, protocol.TypeJobStep, msg); err != nil {
		r.log.Debug("report job step", "step", name, "error", err)
	}
}

func (r *reporter) add(step, stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text = r.redactLocked(text)
	if len(text) > maxLineBytes {
		text = text[:maxLineBytes]
	}
	r.pending = append(r.pending, protocol.LogLine{At: time.Now().UTC(), Step: step, Stream: stream, Text: text})
	if over := len(r.pending) - maxPendingLines; over > 0 {
		r.pending = r.pending[over:]
		r.dropped += over
	}
	if len(r.pending) >= logBatchLines {
		select {
		case r.full <- struct{}{}:
		default:
		}
	}
}

func (r *reporter) flush(ctx context.Context) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	lines, dropped := r.pending, r.dropped
	r.pending, r.dropped = nil, 0
	r.mu.Unlock()
	if dropped > 0 {
		r.log.Warn("dropped job log lines the control plane did not take in time", "lines", dropped)
	}
	for len(lines) > 0 {
		n := min(len(lines), logBatchLines)
		if err := r.send(ctx, protocol.TypeJobLog, protocol.JobLog{JobID: r.jobID, LeaseToken: r.leaseToken, Lines: lines[:n]}); err != nil {
			r.log.Debug("report job log", "lines", len(lines), "error", err)
			return
		}
		lines = lines[n:]
	}
}

// writer returns a writer that turns output into log lines of the step.
func (r *reporter) writer(step, stream string) *lineWriter {
	return &lineWriter{r: r, step: step, stream: stream}
}

// lineWriter splits output into lines at \n and \r and adds each non-empty
// line to the reporter. Close adds the last partial line.
type lineWriter struct {
	r            *reporter
	step, stream string
	buf          []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	for len(w.buf) > maxRawLineBytes {
		w.emit(w.buf[:maxRawLineBytes])
		w.buf = w.buf[maxRawLineBytes:]
	}
	return len(p), nil
}

func (w *lineWriter) Close() error {
	w.emit(w.buf)
	w.buf = nil
	return nil
}

func (w *lineWriter) emit(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	w.r.add(w.step, w.stream, string(line))
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// withTail adds the end of a command's output to its error.
func withTail(err error, tail *tailBuffer) error {
	text := strings.TrimSpace(string(tail.buf))
	if err == nil || text == "" {
		return err
	}
	if i := strings.IndexByte(text, '\n'); len(tail.buf) >= tail.max && i >= 0 {
		text = text[i+1:]
	}
	return fmt.Errorf("%w: %s", err, text)
}
