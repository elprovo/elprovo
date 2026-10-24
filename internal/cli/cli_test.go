package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elprovo/elprovo-next/internal/replay"
	"github.com/elprovo/elprovo-next/internal/testrepo"
)

// app is an elprovo.toml for an app that has nothing to keep running.
const app = "version = 1\nstart = \"echo started > started.txt\"\nready = \"test -f started.txt\"\n"

// featureProof claims that feature.txt says "on".
const featureProof = "claim = \"The feature file says on.\"\nrun = \"grep -qx on feature.txt\"\n"

// printingProof makes the claim of featureProof and prints what it reads.
const printingProof = "claim = \"The feature file says on.\"\nrun = 'echo \"feature is $(cat feature.txt)\"; grep -qx on feature.txt'\n"

type result struct {
	status         int
	stdout, stderr string
	report         replay.Report
}

func run(t *testing.T, ctx context.Context, dir string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := Run(ctx, "test", args, Env{Stdout: &stdout, Stderr: &stderr, Environ: os.Environ(), Dir: dir})
	r := result{status: status, stdout: stdout.String(), stderr: stderr.String()}
	reports, _ := filepath.Glob(filepath.Join(dir, ".elprovo", "runs", "*", "report.json"))
	if len(reports) > 1 {
		t.Fatalf("found %d reports; each test replays once", len(reports))
	}
	if len(reports) == 1 {
		data, err := os.ReadFile(reports[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &r.report); err != nil {
			t.Fatalf("report.json is not valid: %v\n%s", err, data)
		}
	}
	return r
}

func replayIn(t *testing.T, dir string, args ...string) result {
	t.Helper()
	return run(t, context.Background(), dir, append([]string{"replay"}, args...)...)
}

func (r result) proof(t *testing.T, file string) replay.ProofReport {
	t.Helper()
	for _, p := range r.report.Proofs {
		if p.File == file {
			return p
		}
	}
	t.Fatalf("the report has no %s\nstdout:\n%s\nstderr:\n%s", file, r.stdout, r.stderr)
	return replay.ProofReport{}
}

func outcomesOf(runs []replay.RunReport) string {
	var list []string
	for _, run := range runs {
		list = append(list, string(run.Outcome))
	}
	return strings.Join(list, " ")
}

func wantResult(t *testing.T, r result, file string, want replay.Result, wantBase, wantHead string, wantStatus int) {
	t.Helper()
	p := r.proof(t, file)
	if p.Result != want || outcomesOf(p.Base) != wantBase || outcomesOf(p.Head) != wantHead || r.status != wantStatus {
		t.Errorf("%s: result %q, base %q, head %q, exit %d; want %q, base %q, head %q, exit %d\nstdout:\n%s\nstderr:\n%s",
			file, p.Result, outcomesOf(p.Base), outcomesOf(p.Head), r.status,
			want, wantBase, wantHead, wantStatus, r.stdout, r.stderr)
	}
}

func TestReplayVerifiesAProofThatFailsWithoutTheChange(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "off\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "feature.txt": "on\n", "proofs/feature.toml": featureProof})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.Verified, "fail fail", "pass pass", 0)
	wantLines := []string{
		"✓ verified  proofs/feature.toml",
		indent + "The feature file says on.",
		indent + "base: failed 2 of 2   head: passed 2 of 2",
		"1 verified.",
	}
	for _, line := range wantLines {
		if !strings.Contains(r.stdout, line+"\n") {
			t.Errorf("stdout has no line %q:\n%s", line, r.stdout)
		}
	}
}

func TestReplayReportsNoEffectWhenTheProofPassesWithoutTheChange(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "on\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.NoEffect, "pass pass", "pass pass", 1)
}

func TestReplayReportsFailedAndPointsToTheLog(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "off\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":        app,
		"proofs/feature.toml": "claim = \"The feature file says on.\"\nrun = \"echo feature is $(cat feature.txt); grep -qx on feature.txt\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.Failed, "fail fail", "fail fail", 1)
	head := r.proof(t, "proofs/feature.toml").Head[0]
	if head.ExitCode == nil || *head.ExitCode != 1 || head.TimedOut {
		t.Errorf("head run 1 has exit code %v and timed_out %v; want 1 and false", head.ExitCode, head.TimedOut)
	}
	logPath := filepath.Join(".elprovo", "runs", r.replayID(t), "feature", "head-1", "proof.log")
	if !strings.Contains(r.stdout, indent+"log: "+logPath+"\n") {
		t.Errorf("stdout does not point to %s:\n%s", logPath, r.stdout)
	}
	if log, _ := os.ReadFile(filepath.Join(repo.Dir, logPath)); string(log) != "feature is off\n" {
		t.Errorf("proof.log is %q; want %q", log, "feature is off\n")
	}
}

func (r result) replayID(t *testing.T) string {
	t.Helper()
	const prefix = "Report: .elprovo/runs/"
	for _, line := range strings.Split(r.stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSuffix(rest, "/report.json")
		}
	}
	t.Fatalf("stdout has no report line:\n%s", r.stdout)
	return ""
}

func TestBeforeSkipRunsOnTheHeadCommitOnly(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "on\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof + "before = \"skip\"\n"})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.Held, "", "pass pass", 0)
	if !strings.Contains(r.stdout, "not compared with the base: before is skip\n") {
		t.Errorf("stdout does not say why the proof was not compared:\n%s", r.stdout)
	}
	raw, err := os.ReadFile(filepath.Join(repo.Dir, ".elprovo", "runs", r.replayID(t), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"base": []`) {
		t.Errorf("report.json does not give the base runs as an empty list:\n%s", raw)
	}
}

func TestAllReplaysUnchangedProofsOnTheHeadCommitOnly(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"elprovo.toml": app, "feature.txt": "on\n", "proofs/feature.toml": featureProof})
	repo.Commit(map[string]string{"README": "unrelated change\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--all")

	wantResult(t, r, "proofs/feature.toml", replay.Held, "", "pass pass", 0)
	if r.proof(t, "proofs/feature.toml").Claimed {
		t.Error("the proof is claimed; the change does not touch it")
	}
}

func TestAChangeWithoutProofsSelectsNothing(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof, "proofs/helper.sh": "true\n"})
	repo.Commit(map[string]string{"proofs/helper.sh": "false\n"})

	r := replayIn(t, repo.Dir, "--base", base)
	if r.status != 0 || !strings.Contains(r.stdout, "No proof is selected") {
		t.Errorf("exit %d; want 0 and no proof selected\n%s%s", r.status, r.stdout, r.stderr)
	}

	r = replayIn(t, repo.Dir, "--base", base, "--require")
	if r.status != 1 {
		t.Errorf("with --require, exit %d; want 1\n%s%s", r.status, r.stdout, r.stderr)
	}
}

func TestProofsThatDisagreeAreFlaky(t *testing.T) {
	repo := testrepo.New(t)
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COUNTER", counter)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml": app + "pass_env = [\"COUNTER\"]\n",
		// The proof passes on even counts and fails on odd ones.
		"proofs/alternate.toml": "claim = \"The proof alternates.\"\nbefore = \"skip\"\nrun = 'n=$(cat \"$COUNTER\"); echo $((n + 1)) > \"$COUNTER\"; [ $((n % 2)) -eq 0 ]'\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/alternate.toml", replay.Flaky, "", "pass fail", 1)
}

func TestAnAppThatIsNeverReadyIsNotRun(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":        "version = 1\nstart = \"true\"\nready = \"false\"\ntimeouts.ready = \"1s\"\n",
		"proofs/feature.toml": featureProof,
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.ResultNotRun, "not-run", "not-run", 2)
	if reason := r.proof(t, "proofs/feature.toml").Head[0].Reason; reason != "ready did not exit 0 within 1s" {
		t.Errorf("reason is %q; want %q", reason, "ready did not exit 0 within 1s")
	}
	if !strings.Contains(r.stdout, indent+"reason: ready did not exit 0 within 1s\n") {
		t.Errorf("stdout does not give the reason:\n%s", r.stdout)
	}
}

func TestAFailingStartIsNotRunAndStopStillRuns(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":        "version = 1\nstart = \"exit 4\"\nready = \"true\"\nstop = 'touch \"$SPY/stopped\"'\npass_env = [\"SPY\"]\n",
		"proofs/feature.toml": featureProof + "before = \"skip\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.ResultNotRun, "", "not-run", 2)
	if reason := r.proof(t, "proofs/feature.toml").Head[0].Reason; reason != "start exited with status 4" {
		t.Errorf("reason is %q; want %q", reason, "start exited with status 4")
	}
	if _, err := os.Stat(filepath.Join(spy, "stopped")); err != nil {
		t.Error("stop did not run after start failed")
	}
}

func TestATimedOutProofFailsAndItsProcessGroupEnds(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":     app + "pass_env = [\"SPY\"]\n",
		"proofs/slow.toml": "claim = \"The proof is slow.\"\nbefore = \"skip\"\ntimeout = \"1s\"\nrun = 'sleep 60 & echo $! > \"$SPY/pid\"; wait'\n",
	})

	started := time.Now()
	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/slow.toml", replay.Failed, "", "fail", 1)
	head := r.proof(t, "proofs/slow.toml").Head[0]
	if !head.TimedOut || head.ExitCode != nil {
		t.Errorf("timed_out %v and exit_code %v; want true and null", head.TimedOut, head.ExitCode)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("the replay took %v; the proof's timeout is 1s", elapsed)
	}
	wantGone(t, filepath.Join(spy, "pid"))
}

// wantGone checks that the process whose ID is in pidFile no longer runs.
func wantGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the proof did not write its child's process ID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("process %d still runs after its command timed out", pid)
	}
}

func TestAFailingStopEndsTheReplay(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":  app + "stop = \"exit 3\"\n",
		"proofs/a.toml": "claim = \"A holds.\"\nbefore = \"skip\"\nrun = \"true\"\n",
		"proofs/b.toml": "claim = \"B holds.\"\nbefore = \"skip\"\nrun = \"true\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "stop exited with status 3" {
		t.Errorf("exit %d, stopped_early %q; want 2 and %q\n%s", r.status, r.report.StoppedEarly, "stop exited with status 3", r.stdout)
	}
	if a := r.proof(t, "proofs/a.toml"); a.Result != replay.Unfinished || len(a.Head) != 1 {
		t.Errorf("proofs/a.toml has result %q and %d head runs; want no result and 1 run", a.Result, len(a.Head))
	}
	if b := r.proof(t, "proofs/b.toml"); len(b.Head) != 0 {
		t.Errorf("proofs/b.toml has %d head runs; want 0", len(b.Head))
	}
}

func TestAnInterruptRunsStopAndEndsTheReplay(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":     app + "pass_env = [\"SPY\"]\nstop = 'touch \"$SPY/stopped\"'\n",
		"proofs/slow.toml": "claim = \"The proof is slow.\"\nbefore = \"skip\"\nrun = 'sleep 60 & echo $! > \"$SPY/pid\"; wait'\n",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(spy, "pid")); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	r := run(t, ctx, repo.Dir, "replay", "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "interrupted" {
		t.Errorf("exit %d, stopped_early %q; want 2 and interrupted\n%s%s", r.status, r.report.StoppedEarly, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(spy, "stopped")); err != nil {
		t.Error("stop did not run after the interrupt")
	}
	if p := r.proof(t, "proofs/slow.toml"); len(p.Head) != 0 || p.Result != replay.Unfinished {
		t.Errorf("the report has %d head runs and result %q; the interrupted run must not count", len(p.Head), p.Result)
	}
	wantGone(t, filepath.Join(spy, "pid"))
}

func TestCommandsGetOnlyTheRunsEnvironmentAndAFreshCopy(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	t.Setenv("PARENT_SECRET", "must not leak")
	t.Setenv("LANG", "en_US.UTF-8")
	base := repo.Commit(map[string]string{
		"src/proofs/keep.txt": "nested\n",
		"proofs/stale.txt":    "only in the base commit\n",
	})
	repo.Commit(map[string]string{
		"elprovo.toml": app + "pass_env = [\"SPY\"]\nenv.FROM_ENV = \"plain\"\n",
		"proofs/spy.toml": "claim = \"The spy records what a run sees.\"\nrun = '''\n" +
			"d=\"$SPY/$ELPROVO_RUN_ID\"; mkdir \"$d\"\n" +
			"env | sort > \"$d/env\"; pwd > \"$d/pwd\"; ls -A \"$HOME\" > \"$d/home\"\n" +
			"ls -A . proofs src/proofs > \"$d/files\"\n" +
			"echo \"$ELPROVO_RUN_ID\" > \"$ELPROVO_ARTIFACTS/seen.txt\"\n'''\n",
	}, "proofs/stale.txt")

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")
	if r.status != 0 && r.status != 1 {
		t.Fatalf("exit %d\n%s%s", r.status, r.stdout, r.stderr)
	}

	runs, _ := os.ReadDir(spy)
	if len(runs) != 2 {
		t.Fatalf("the spy saw %d runs; want 2, one on each commit", len(runs))
	}
	homes := map[string]bool{}
	for _, entry := range runs {
		d := filepath.Join(spy, entry.Name())
		env := readLines(t, filepath.Join(d, "env"))
		vars := map[string]string{}
		for _, line := range env {
			name, value, _ := strings.Cut(line, "=")
			vars[name] = value
		}
		if vars["HOME"] == "" || vars["HOME"] == os.Getenv("HOME") || homes[vars["HOME"]] {
			t.Errorf("HOME is %q; want a new directory for each run", vars["HOME"])
		}
		homes[vars["HOME"]] = true
		if vars["TMPDIR"] == "" || vars["TMPDIR"] == os.Getenv("TMPDIR") {
			t.Errorf("TMPDIR is %q; want the run's own directory", vars["TMPDIR"])
		}
		if vars["FROM_ENV"] != "plain" || vars["SPY"] != spy || vars["ELPROVO_RUN_ID"] != entry.Name() {
			t.Errorf("env, pass_env or ELPROVO_RUN_ID is wrong: %v", vars)
		}
		if _, ok := vars["PARENT_SECRET"]; ok {
			t.Error("a variable from the environment of elprovo replay leaked into the run")
		}
		if vars["LANG"] != "en_US.UTF-8" || vars["USER"] != os.Getenv("USER") {
			t.Errorf("LANG is %q and USER is %q; want them from the environment of elprovo replay", vars["LANG"], vars["USER"])
		}
		artifacts := vars["ELPROVO_ARTIFACTS"]
		if strings.Contains(artifacts, "/base-") || strings.Contains(artifacts, "/head-") || strings.Contains(artifacts, ".elprovo") {
			t.Errorf("ELPROVO_ARTIFACTS is %q; it must not show the commit or lead to the output directory", artifacts)
		}
		for name := range vars {
			if strings.HasPrefix(name, "ELPROVO_") && name != "ELPROVO_RUN_ID" && name != "ELPROVO_ARTIFACTS" {
				t.Errorf("the run got %s; only ELPROVO_RUN_ID and ELPROVO_ARTIFACTS are allowed", name)
			}
		}
		if pwd := readLines(t, filepath.Join(d, "pwd")); len(pwd) != 1 || filepath.Base(pwd[0]) != "elprovo-"+entry.Name() {
			t.Errorf("the working directory is %v; want elprovo-%s", pwd, entry.Name())
		}
		if home := readLines(t, filepath.Join(d, "home")); len(home) != 0 {
			t.Errorf("HOME holds %v; want it empty", home)
		}
		files := strings.Join(readLines(t, filepath.Join(d, "files")), " ")
		if strings.Contains(files, ".git") || strings.Contains(files, "stale.txt") || !strings.Contains(files, "keep.txt") {
			t.Errorf("the copy holds %q; want no .git, no stale.txt from the base proofs, and src/proofs/keep.txt", files)
		}
	}
	for _, side := range []string{"base-1", "head-1"} {
		seen := filepath.Join(repo.Dir, ".elprovo", "runs", r.replayID(t), "spy", side, "artifacts", "seen.txt")
		if _, err := os.Stat(seen); err != nil {
			t.Errorf("the %s artifact is not in the output directory: %v", side, err)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestInvalidInputExitsTwoWithoutRunning(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		args        []string
		wantInError string
	}{
		{"unknown field", map[string]string{"elprovo.toml": app + "stopp = \"x\"\n", "proofs/feature.toml": featureProof}, nil, `unknown field "stopp"`},
		{"invalid proof name", map[string]string{"elprovo.toml": app, "proofs/Feature.toml": featureProof}, nil, "proofs/Feature.toml: the name before .toml"},
		{"reserved pass_env name", map[string]string{"elprovo.toml": app + "pass_env = [\"ELPROVO_TEST_UNSET\"]\n", "proofs/feature.toml": featureProof}, nil, "must not start with ELPROVO_"},
		{"unset pass_env variable", map[string]string{"elprovo.toml": app + "pass_env = [\"UNSET_FOR_ELPROVO_TEST\"]\n", "proofs/feature.toml": featureProof}, nil, "pass_env names UNSET_FOR_ELPROVO_TEST, which is not set"},
		{"no elprovo.toml", map[string]string{"proofs/feature.toml": featureProof}, nil, "has no elprovo.toml"},
		{"YAML proof file", map[string]string{"elprovo.toml": app, "proofs/feature.yml": "claim: The feature file says on.\nrun: grep -qx on feature.txt\n"}, nil, "proofs/feature.yml: Elprovo reads proof files that end in .toml"},
		{"unknown proof argument", map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof}, []string{"proofs/missing.toml"}, `"proofs/missing.toml" is not a proof file`},
		{"too many runs", map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof}, []string{"--runs", "11"}, "--runs is 11"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testrepo.New(t)
			base := repo.Commit(map[string]string{"README": "base\n"})
			repo.Commit(tt.files)
			r := replayIn(t, repo.Dir, append([]string{"--base", base}, tt.args...)...)
			if r.status != 2 || !strings.Contains(r.stderr, tt.wantInError) {
				t.Errorf("exit %d, stderr %q; want 2 and an error that contains %q", r.status, r.stderr, tt.wantInError)
			}
			if _, err := os.Stat(filepath.Join(repo.Dir, ".elprovo")); err == nil {
				t.Error("an invalid replay made the output directory")
			}
		})
	}
}

func TestDryRunRunsNoCommand(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":        "version = 1\nstart = 'touch \"$SPY/started\"'\nready = \"true\"\npass_env = [\"SPY\"]\n",
		"proofs/feature.toml": featureProof,
		"proofs/kept.toml":    "claim = \"Kept.\"\nbefore = \"skip\"\nrun = \"true\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	if r.status != 0 {
		t.Errorf("exit %d; want 0\n%s%s", r.status, r.stdout, r.stderr)
	}
	for _, line := range []string{"proofs/feature.toml  base and head", "proofs/kept.toml  head only: before is skip", "Dry run: no command ran."} {
		if !strings.Contains(r.stdout, line+"\n") {
			t.Errorf("stdout has no line %q:\n%s", line, r.stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(spy, "started")); err == nil {
		t.Error("a dry run ran start")
	}
}

func TestVersionPrintsTheVersion(t *testing.T) {
	r := run(t, context.Background(), t.TempDir(), "version")
	if r.status != 0 || r.stdout != "elprovo test\n" {
		t.Errorf("exit %d, stdout %q; want 0 and %q", r.status, r.stdout, "elprovo test\n")
	}
}

func TestAProcessThatIgnoresSIGTERMIsKilled(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml": app + "pass_env = [\"SPY\"]\n",
		// The child ignores SIGTERM, and the shell that started it does not.
		"proofs/stubborn.toml": "claim = \"The proof leaves a stubborn child.\"\nbefore = \"skip\"\ntimeout = \"1s\"\n" +
			"run = '''(trap '' TERM; exec sleep 300) & echo $! > \"$SPY/pid\"; wait'''\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/stubborn.toml", replay.Failed, "", "fail", 1)
	wantGone(t, filepath.Join(spy, "pid"))
}

func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	tests := []struct {
		name, file, content string
	}{
		{"in a claim", "proofs/feature.toml", "claim = \"Looks fine\\e[2K\\u0007\"\nrun = \"true\"\n"},
		{"as a line separator in a claim", "proofs/feature.toml", "claim = \"one\\u2028two\"\nrun = \"true\"\n"},
		{"in an unknown field", "proofs/feature.toml", "claim = \"c\"\nrun = \"true\"\n\"\\e]52;c;owned\\u0007\" = \"x\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testrepo.New(t)
			base := repo.Commit(map[string]string{"README": "base\n"})
			repo.Commit(map[string]string{"elprovo.toml": app, tt.file: tt.content})

			r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

			if r.status != 2 {
				t.Errorf("exit %d; want 2 for an invalid proof file", r.status)
			}
			for _, out := range []string{r.stdout, r.stderr} {
				if strings.ContainsAny(out, "\x1b\a\u2028") {
					t.Errorf("a control character reached the output: %q", out)
				}
			}
		})
	}
}

func TestAnInterruptDuringTheCopyIsReportedAsAnInterrupt(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	// A smudge filter that waits makes the copy slow enough to interrupt.
	repo.Git("config", "filter.slow.smudge", `touch "$SPY/copying"; sleep 30; cat`)
	base := repo.Commit(map[string]string{".gitattributes": "slow.txt filter=slow\n", "slow.txt": "data\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof + "before = \"skip\"\n"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(spy, "copying")); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	r := run(t, ctx, repo.Dir, "replay", "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "interrupted" {
		t.Errorf("exit %d, stopped_early %q; want 2 and interrupted\n%s%s", r.status, r.report.StoppedEarly, r.stdout, r.stderr)
	}
}

func TestAProofThatIsFlakyOnTheBasePointsToTheFailingBaseLog(t *testing.T) {
	repo := testrepo.New(t)
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COUNTER", counter)
	base := repo.Commit(map[string]string{"feature.txt": "off\n"})
	repo.Commit(map[string]string{
		"elprovo.toml": app + "pass_env = [\"COUNTER\"]\n",
		"feature.txt":  "on\n",
		// With the change, the proof passes. Without it, it passes on the
		// first base run and fails on the second.
		"proofs/feature.toml": "claim = \"The feature is on, or the count is even.\"\nrun = '''\n" +
			"grep -qx on feature.txt && exit 0\n" +
			"n=$(cat \"$COUNTER\"); echo $((n + 1)) > \"$COUNTER\"; [ $((n % 2)) -eq 0 ]\n'''\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.Flaky, "pass fail", "pass pass", 1)
	logPath := filepath.Join(".elprovo", "runs", r.replayID(t), "feature", "base-2", "proof.log")
	if !strings.Contains(r.stdout, indent+"log: "+logPath+"\n") {
		t.Errorf("stdout does not point to %s:\n%s", logPath, r.stdout)
	}
}

func TestNotRunGivesTheReasonOfTheFirstRunThatDidNotRun(t *testing.T) {
	repo := testrepo.New(t)
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COUNTER", counter)
	// The base commit's app never starts. The head commit's app is ready
	// only the first time.
	base := repo.Commit(map[string]string{"elprovo.toml": "version = 1\nstart = \"exit 5\"\nready = \"true\"\n"})
	repo.Commit(map[string]string{
		"elprovo.toml": "version = 1\nstart = \"true\"\npass_env = [\"COUNTER\"]\ntimeouts.ready = \"1s\"\n" +
			"ready = 'n=$(cat \"$COUNTER\"); echo $((n + 1)) > \"$COUNTER\"; [ \"$n\" -eq 0 ]'\n",
		"proofs/feature.toml": "claim = \"The proof passes.\"\nrun = \"true\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.ResultNotRun, "not-run not-run", "pass not-run", 2)
	if !strings.Contains(r.stdout, indent+"reason: start exited with status 5\n") {
		t.Errorf("stdout does not give the reason of base run 1, the first run that did not run:\n%s", r.stdout)
	}
}

func TestBaseRunsUseTheBaseConfigAndTheCommitsAlternate(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{
		"elprovo.toml": "version = 1\nstart = 'echo base >> \"$SPY/order\"'\nready = \"true\"\npass_env = [\"SPY\"]\n",
		"feature.txt":  "off\n",
	})
	repo.Commit(map[string]string{
		"elprovo.toml":        "version = 1\nstart = 'echo head >> \"$SPY/order\"'\nready = \"true\"\npass_env = [\"SPY\"]\n",
		"feature.txt":         "on\n",
		"proofs/feature.toml": featureProof,
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.toml", replay.Verified, "fail fail", "pass pass", 0)
	if order := strings.Join(readLines(t, filepath.Join(spy, "order")), " "); order != "base head base head" {
		t.Errorf("the runs started in the order %q; want %q", order, "base head base head")
	}
}

func TestReadyIsRunAgainUntilItSucceeds(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		// ready fails the first time and succeeds the second time.
		"elprovo.toml":        "version = 1\nstart = \"true\"\nready = \"test -f tried || { touch tried; false; }\"\n",
		"proofs/feature.toml": "claim = \"The proof passes.\"\nbefore = \"skip\"\nrun = \"true\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.Held, "", "pass", 0)
}

func TestTheDefaultBaseIsTheMergeBaseWithOriginHead(t *testing.T) {
	repo := testrepo.New(t)
	fork := repo.Commit(map[string]string{"elprovo.toml": app, "feature.txt": "off\n"})
	repo.Git("switch", "-q", "-c", "change")
	repo.Commit(map[string]string{"feature.txt": "on\n", "proofs/feature.toml": featureProof})
	// main moves on after the branch was made.
	repo.Git("switch", "-q", "main")
	upstream := repo.Commit(map[string]string{"README": "later on main\n"})
	repo.Git("switch", "-q", "change")
	repo.Git("update-ref", "refs/remotes/origin/main", upstream)
	repo.Git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	r := replayIn(t, repo.Dir, "--runs", "1")

	if r.report.Base != fork {
		t.Errorf("the base commit is %s; want the merge base %s, not origin/HEAD %s", r.report.Base, fork, upstream)
	}
	wantResult(t, r, "proofs/feature.toml", replay.Verified, "fail", "pass", 0)
}

func TestProofArgumentsAndOutAreRelativeToTheWorkingDirectory(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"sub/README": "a subdirectory\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": featureProof + "before = \"skip\"\n", "feature.txt": "on\n"})
	sub := filepath.Join(repo.Dir, "sub")

	var stdout, stderr bytes.Buffer
	status := Run(context.Background(), "test", []string{"replay", "--base", base, "--runs", "1", "--out", "../out", "../proofs/feature.toml"},
		Env{Stdout: &stdout, Stderr: &stderr, Environ: os.Environ(), Dir: sub})

	if status != 0 || !strings.Contains(stdout.String(), "✓ held      proofs/feature.toml\n") {
		t.Errorf("exit %d; want 0 and the proof held\n%s%s", status, stdout.String(), stderr.String())
	}
	if reports, _ := filepath.Glob(filepath.Join(repo.Dir, "out", "runs", "*", "report.json")); len(reports) != 1 {
		t.Errorf("found %d reports in the out directory; want 1", len(reports))
	}
}

func TestAllWithoutProofFilesSaysSo(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{"elprovo.toml": app})

	r := replayIn(t, repo.Dir, "--base", base, "--all")

	if r.status != 0 || !strings.Contains(r.stdout, "No proof is selected: the head commit has no proof file.\n") {
		t.Errorf("exit %d; want 0 and a message that the head commit has no proof file\n%s", r.status, r.stdout)
	}
}

func TestTheCopyAppliesCheckoutFilters(t *testing.T) {
	repo := testrepo.New(t)
	// Git LFS fills files through a checkout filter like this one, which
	// writes the stored text in capitals.
	repo.Git("config", "filter.upper.clean", "cat")
	repo.Git("config", "filter.upper.smudge", "tr a-z A-Z")
	base := repo.Commit(map[string]string{".gitattributes": "data.txt filter=upper\n", "data.txt": "hello\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":         app,
		"proofs/filtered.toml": "claim = \"The copy holds the filtered file.\"\nbefore = \"skip\"\nrun = \"grep -qx HELLO data.txt\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/filtered.toml", replay.Held, "", "pass", 0)
}

func TestAGitLFSFileIsCopiedWithItsContent(t *testing.T) {
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs is not installed")
	}
	repo := testrepo.New(t)
	repo.Git("lfs", "install", "--local")
	repo.Git("lfs", "track", "data.bin")
	base := repo.Commit(map[string]string{"data.bin": "real content\n"})
	if pointer := repo.Git("cat-file", "-p", base+":data.bin"); !strings.Contains(pointer, "git-lfs.github.com/spec") {
		t.Fatalf("the commit holds %q; want a Git LFS pointer", pointer)
	}
	repo.Commit(map[string]string{
		"elprovo.toml":    app,
		"proofs/lfs.toml": "claim = \"The copy holds the file's content, not its Git LFS pointer.\"\nbefore = \"skip\"\nrun = \"grep -qx 'real content' data.bin\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/lfs.toml", replay.Held, "", "pass", 0)
}

func TestVerifiedShowsWhatTheBaseRunPrinted(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "off\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "feature.txt": "on\n", "proofs/feature.toml": printingProof})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.Verified, "fail", "pass", 0)
	if !strings.Contains(r.stdout, indent+"base run 1 ended with: feature is off\n") {
		t.Errorf("stdout does not show what the base run printed:\n%s", r.stdout)
	}
	p := r.proof(t, "proofs/feature.toml")
	if p.Base[0].LastLine != "feature is off" || p.Head[0].LastLine != "" {
		t.Errorf("last_line is %q on the base and %q on the head; want %q and empty", p.Base[0].LastLine, p.Head[0].LastLine, "feature is off")
	}
}

func TestFailedShowsWhatTheHeadRunPrinted(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "off\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": printingProof})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.Failed, "fail", "fail", 1)
	if !strings.Contains(r.stdout, indent+"head run 1 ended with: feature is off\n") {
		t.Errorf("stdout does not show what the head run printed:\n%s", r.stdout)
	}
}

func TestNotRunShowsWhatStartPrinted(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":        "version = 1\nstart = \"echo cannot build; exit 4\"\nready = \"true\"\n",
		"proofs/feature.toml": featureProof + "before = \"skip\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.ResultNotRun, "", "not-run", 2)
	if got := r.proof(t, "proofs/feature.toml").Head[0].LastLine; got != "cannot build" {
		t.Errorf("last_line is %q; want %q", got, "cannot build")
	}
	if !strings.Contains(r.stdout, indent+"head run 1 ended with: cannot build\n") {
		t.Errorf("stdout does not show what start printed:\n%s", r.stdout)
	}
}

func TestASilentFailureSaysThatTheRunPrintedNothing(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": "claim = \"Nothing.\"\nbefore = \"skip\"\nrun = \"false\"\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.Failed, "", "fail", 1)
	if !strings.Contains(r.stdout, indent+"head run 1 printed nothing\n") {
		t.Errorf("stdout does not say that the run printed nothing:\n%s", r.stdout)
	}
	if got := r.proof(t, "proofs/feature.toml").Head[0].LastLine; got != "" {
		t.Errorf("last_line is %q; want empty", got)
	}
}

func TestControlCharactersInALogLineAreEscaped(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{"elprovo.toml": app, "proofs/feature.toml": "claim = \"Escaped.\"\nbefore = \"skip\"\nrun = '''printf 'gone\\033[2K\\n'; exit 1'''\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.Failed, "", "fail", 1)
	if got := r.proof(t, "proofs/feature.toml").Head[0].LastLine; got != `gone\x1b[2K` {
		t.Errorf("last_line is %q; want %q", got, `gone\x1b[2K`)
	}
	if strings.Contains(r.stdout, "\x1b") {
		t.Errorf("a control character reached stdout: %q", r.stdout)
	}
}

func TestNotRunShowsWhatReadyPrinted(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml":        "version = 1\nstart = \"true\"\nready = \"echo connection refused; false\"\ntimeouts.ready = \"1s\"\n",
		"proofs/feature.toml": featureProof + "before = \"skip\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.toml", replay.ResultNotRun, "", "not-run", 2)
	if got := r.proof(t, "proofs/feature.toml").Head[0].LastLine; got != "connection refused" {
		t.Errorf("last_line is %q; want %q", got, "connection refused")
	}
}
