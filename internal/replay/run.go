package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/elprovo/elprovo-next/internal/config"
)

// readyInterval is the pause between two runs of ready.
const readyInterval = time.Second

const (
	// lastLineBytes is how much of the end of a log Elprovo reads to find
	// its last line.
	lastLineBytes = 64 << 10
	// maxLastLine is the most characters of a log line that a report keeps.
	maxLastLine = 200
)

type runner struct {
	plan   Plan
	opts   Options
	dir    string
	limits Limits
}

// replayProof does every run of one proof, alternating the commits, and adds
// the runs to report. It returns why the replay must stop, or "".
func (r runner) replayProof(ctx context.Context, p Planned, report *ProofReport) string {
	sides := []string{"head"}
	if p.OnBase {
		sides = []string{"base", "head"}
	}
	for n := 1; n <= r.opts.Runs; n++ {
		for _, side := range sides {
			if ctx.Err() != nil {
				return "interrupted"
			}
			run, stop := r.runOnce(ctx, p, side, n)
			if run != nil {
				if side == "base" {
					report.Base = append(report.Base, *run)
				} else {
					report.Head = append(report.Head, *run)
				}
				if r.opts.RunDone != nil {
					r.opts.RunDone(*report, side, *run)
				}
			}
			if stop != "" {
				return stop
			}
		}
	}
	return ""
}

// runOnce does the steps of section 3.2 of SPEC.md for run n of proof p on
// one side. It returns the run, or nil when the run did not end because of an
// interrupt, and why the replay must stop, or "".
func (r runner) runOnce(ctx context.Context, p Planned, side string, n int) (*RunReport, string) {
	started := time.Now()
	commit, cfg := r.plan.Head, r.plan.headConfig
	if side == "base" {
		commit = r.plan.Base
		if r.plan.baseConfig != nil {
			cfg = *r.plan.baseConfig
		}
	}

	// cannot ends the replay because Elprovo itself failed. When the replay
	// was interrupted, the failure is a result of the interrupt.
	cannot := func(format string, args ...any) (*RunReport, string) {
		if ctx.Err() != nil {
			return nil, "interrupted"
		}
		return nil, fmt.Sprintf(format, args...)
	}

	runDir := filepath.Join(r.dir, p.Name, fmt.Sprintf("%s-%d", side, n))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return cannot("could not make %s: %v", runDir, err)
	}
	id := randomID()
	scratch, err := os.MkdirTemp("", "elprovo-")
	if err != nil {
		return cannot("could not make a temporary directory: %v", err)
	}
	defer r.remove(scratch)
	work := filepath.Join(scratch, "elprovo-"+id)
	home := filepath.Join(scratch, "home")
	tmp := filepath.Join(scratch, "tmp")
	// The artifacts directory is in the run's directory, so its path does
	// not show the commit, and it does not lead to the output directory.
	artifacts := filepath.Join(scratch, "artifacts")
	for _, dir := range []string{work, home, tmp, artifacts} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return cannot("could not make %s: %v", dir, err)
		}
	}
	inProofs := func(file string) bool { return strings.HasPrefix(file, config.ProofDir+"/") }
	if err := r.plan.repo.Checkout(ctx, commit, work, scratch, func(f string) bool { return !inProofs(f) }); err != nil {
		return cannot("could not copy commit %s: %v", short(commit), err)
	}
	if err := r.plan.repo.Checkout(ctx, r.plan.Head, work, scratch, inProofs); err != nil {
		return cannot("could not copy the proofs of commit %s: %v", short(r.plan.Head), err)
	}

	env := commandEnv(r.opts.Environ, cfg, home, tmp, id, artifacts)
	command := func(ctx context.Context, name, line string, timeout time.Duration) (commandResult, error) {
		return runCommand(ctx, line, work, env, filepath.Join(runDir, name+".log"), timeout)
	}
	run := &RunReport{Run: n, Outcome: NotRun, Dir: filepath.Join(p.Name, fmt.Sprintf("%s-%d", side, n)), LogDir: runDir}
	stop := ""
	interrupted := false
	// decidedBy names the command whose log explains a fail or not-run
	// outcome.
	decidedBy := ""

	result, err := command(ctx, "start", cfg.Start, cfg.StartTimeout)
	switch {
	case err != nil:
		stop = fmt.Sprintf("could not run start: %v", err)
	case result.interrupted:
		interrupted = true
	case !result.ok():
		run.Reason = result.describe("start", cfg.StartTimeout)
		decidedBy = "start"
	default:
		var ready bool
		ready, interrupted, run.Reason, err = r.waitReady(ctx, command, cfg.Ready, cfg.ReadyTimeout)
		if err != nil {
			stop = fmt.Sprintf("could not run ready: %v", err)
		}
		if run.Reason != "" {
			decidedBy = "ready"
		}
		if ready {
			result, err := command(ctx, "proof", p.Run, p.Timeout)
			switch {
			case err != nil:
				stop = fmt.Sprintf("could not run the proof: %v", err)
			case result.interrupted:
				interrupted = true
			default:
				run.Outcome = Fail
				if result.ok() {
					run.Outcome = Pass
				} else {
					decidedBy = "proof"
				}
				run.TimedOut = result.timedOut
				if result.exitCode >= 0 {
					code := result.exitCode
					run.ExitCode = &code
				}
			}
		}
	}

	if cfg.Stop != "" {
		// stop runs after an interrupt too, so it must not inherit ctx.
		result, err := command(context.WithoutCancel(ctx), "stop", cfg.Stop, cfg.StopTimeout)
		switch {
		case err != nil:
			stop = fmt.Sprintf("could not run stop: %v", err)
		case !result.ok() && stop == "":
			stop = result.describe("stop", cfg.StopTimeout)
		}
	}
	run.Seconds = math.Round(time.Since(started).Seconds()*10) / 10

	// The cut drops the end of a long log, so the last line comes first.
	if decidedBy != "" {
		log := filepath.Join(runDir, decidedBy+".log")
		if run.LastLine, err = lastLine(log); err != nil {
			r.warn(fmt.Sprintf("could not read the last line of %s: %v", log, err))
		}
	}
	r.cutLogs(runDir)
	run.ArtifactsDropped = r.keepArtifacts(artifacts, filepath.Join(runDir, "artifacts"))

	if interrupted || (stop != "" && ctx.Err() != nil) {
		return nil, "interrupted"
	}
	return run, stop
}

// waitReady runs ready once a second until it exits 0 or timeout passes.
func (r runner) waitReady(ctx context.Context, command func(context.Context, string, string, time.Duration) (commandResult, error), line string, timeout time.Duration) (ready, interrupted bool, reason string, err error) {
	deadline := time.Now().Add(timeout)
	notReady := fmt.Sprintf("ready did not exit 0 within %s", formatDuration(timeout))
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, false, notReady, nil
		}
		result, err := command(ctx, "ready", line, remaining)
		switch {
		case err != nil:
			return false, false, "", err
		case result.interrupted:
			return false, true, "", nil
		case result.ok():
			return true, false, "", nil
		case result.timedOut:
			return false, false, notReady, nil
		}
		select {
		case <-ctx.Done():
			return false, true, "", nil
		case <-time.After(min(readyInterval, time.Until(deadline))):
		}
	}
}

// commandEnv builds the environment of section 4 of SPEC.md. A later
// variable replaces an earlier one with the same name.
func commandEnv(environ []string, cfg config.Repo, home, tmp, id, artifacts string) []string {
	parent := envMap(environ)
	var names []string
	values := map[string]string{}
	set := func(name, value string) {
		if _, ok := values[name]; !ok {
			names = append(names, name)
		}
		values[name] = value
	}
	for _, name := range []string{"PATH", "USER", "LANG"} {
		if value, ok := parent[name]; ok {
			set(name, value)
		}
	}
	set("HOME", home)
	set("TMPDIR", tmp)
	envNames := make([]string, 0, len(cfg.Env))
	for name := range cfg.Env {
		envNames = append(envNames, name)
	}
	slices.Sort(envNames)
	for _, name := range envNames {
		set(name, cfg.Env[name])
	}
	for _, name := range cfg.PassEnv {
		set(name, parent[name])
	}
	set("ELPROVO_RUN_ID", id)
	set("ELPROVO_ARTIFACTS", artifacts)

	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env
}

// cutLogs cuts each log in dir that is longer than the limit, and adds a
// line that says so.
func (r runner) cutLogs(dir string) {
	logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for _, log := range logs {
		info, err := os.Stat(log)
		if err != nil || info.Size() <= r.limits.LogBytes {
			continue
		}
		if err := os.Truncate(log, r.limits.LogBytes); err != nil {
			r.warn(fmt.Sprintf("could not cut %s: %v", log, err))
			continue
		}
		f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			r.warn(fmt.Sprintf("could not mark %s as cut: %v", log, err))
			continue
		}
		fmt.Fprintf(f, "\n[elprovo: this log was cut to its first %d bytes]\n", r.limits.LogBytes)
		f.Close()
	}
}

// lastLine returns the last non-empty line of the log at path, cut to
// maxLastLine characters, with control characters escaped. It returns ""
// when the log has no such line.
func lastLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := max(0, info.Size()-lastLineBytes)
	tail := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	lines := strings.Split(strings.ToValidUTF8(string(tail), "\uFFFD"), "\n")
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > maxLastLine {
			line = string(runes[:maxLastLine])
		}
		return config.EscapeControl(line), nil
	}
	return "", nil
}

// keepArtifacts moves the run's artifacts from to the output directory to.
// It drops them instead, and reports true, when they are over the limit or
// when Elprovo cannot measure them. to exists afterwards in every case.
func (r runner) keepArtifacts(from, to string) bool {
	dropped := false
	size, err := dirSize(from)
	switch {
	case err != nil:
		r.warn(fmt.Sprintf("could not measure the artifacts for %s, so they were dropped: %v", to, err))
		dropped = true
	case size > r.limits.ArtifactsBytes:
		dropped = true
	default:
		if err := moveDir(from, to); err != nil {
			r.warn(fmt.Sprintf("could not keep the artifacts in %s, so they were dropped: %v", to, err))
			r.remove(to)
			dropped = true
		}
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		r.warn(fmt.Sprintf("could not make %s: %v", to, err))
	}
	return dropped
}

// moveDir renames from to to, and copies it when the two are on different
// file systems. It copies directories, regular files and symbolic links,
// and leaves out other kinds of file, such as sockets.
func moveDir(from, to string) error {
	if os.Rename(from, to) == nil {
		return nil
	}
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target)
		}
		return nil
	})
}

func copyFile(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func dirSize(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// remove deletes path. Some tools, such as the Go module cache, make
// directories read-only, so when the first try fails it makes every
// directory writable and tries again.
func (r runner) remove(path string) {
	if os.RemoveAll(path) == nil {
		return
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	if err := os.RemoveAll(path); err != nil {
		r.warn(fmt.Sprintf("could not delete %s: %v", path, err))
	}
}

func (r runner) warn(message string) {
	if r.opts.Warn != nil {
		r.opts.Warn(message)
	}
}
