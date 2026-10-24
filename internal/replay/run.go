package replay

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/elprovo/elprovo/internal/config"
)

// readyInterval is the pause between two runs of ready.
const readyInterval = time.Second

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

	runDir := filepath.Join(r.dir, p.Name, fmt.Sprintf("%s-%d", side, n))
	artifacts := filepath.Join(runDir, "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		return nil, fmt.Sprintf("could not make %s: %v", artifacts, err)
	}
	id := randomID()
	scratch, err := os.MkdirTemp("", "elprovo-")
	if err != nil {
		return nil, fmt.Sprintf("could not make a temporary directory: %v", err)
	}
	defer r.remove(scratch)
	work := filepath.Join(scratch, "elprovo-"+id)
	home := filepath.Join(scratch, "home")
	tmp := filepath.Join(scratch, "tmp")
	for _, dir := range []string{work, home, tmp} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, fmt.Sprintf("could not make %s: %v", dir, err)
		}
	}
	inProofs := func(file string) bool { return strings.HasPrefix(file, config.ProofDir+"/") }
	if err := r.plan.repo.Checkout(ctx, commit, work, scratch, func(f string) bool { return !inProofs(f) }); err != nil {
		return nil, fmt.Sprintf("could not copy commit %s: %v", short(commit), err)
	}
	if err := r.plan.repo.Checkout(ctx, r.plan.Head, work, scratch, inProofs); err != nil {
		return nil, fmt.Sprintf("could not copy the proofs of commit %s: %v", short(r.plan.Head), err)
	}

	env := commandEnv(r.opts.Environ, cfg, home, tmp, id, artifacts)
	command := func(ctx context.Context, name, line string, timeout time.Duration) (commandResult, error) {
		return runCommand(ctx, line, work, env, filepath.Join(runDir, name+".log"), timeout)
	}
	run := &RunReport{Run: n, Outcome: NotRun, Dir: filepath.Join(p.Name, fmt.Sprintf("%s-%d", side, n)), LogDir: runDir}
	stop := ""
	interrupted := false

	result, err := command(ctx, "start", cfg.Start, cfg.StartTimeout)
	switch {
	case err != nil:
		stop = fmt.Sprintf("could not run start: %v", err)
	case result.interrupted:
		interrupted = true
	case !result.ok():
		run.Reason = result.describe("start", cfg.StartTimeout)
	default:
		var ready bool
		ready, interrupted, run.Reason, err = r.waitReady(ctx, command, cfg.Ready, cfg.ReadyTimeout)
		if err != nil {
			stop = fmt.Sprintf("could not run ready: %v", err)
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

	r.cutLogs(runDir)
	if size, err := dirSize(artifacts); err == nil && size > r.limits.ArtifactsBytes {
		r.remove(artifacts)
		run.ArtifactsDropped = os.MkdirAll(artifacts, 0o755) == nil
	}
	run.Seconds = math.Round(time.Since(started).Seconds()*10) / 10

	if interrupted {
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
