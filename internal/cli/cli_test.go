package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elprovo/elprovo/internal/replay"
	"github.com/elprovo/elprovo/internal/testrepo"
)

// app is an elprovo.yml for an app that has nothing to keep running.
const app = "version: 1\nstart: echo started > started.txt\nready: test -f started.txt\n"

// featureProof claims that feature.txt says "on".
const featureProof = "claim: The feature file says on.\nrun: grep -qx on feature.txt\n"

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
	repo.Commit(map[string]string{"elprovo.yml": app, "feature.txt": "on\n", "proofs/feature.yml": featureProof})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.yml", replay.Verified, "fail fail", "pass pass", 0)
	wantLines := []string{
		"✓ verified  proofs/feature.yml",
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
	repo.Commit(map[string]string{"elprovo.yml": app, "proofs/feature.yml": featureProof})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.yml", replay.NoEffect, "pass pass", "pass pass", 1)
}

func TestReplayReportsFailedAndPointsToTheLog(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"feature.txt": "off\n"})
	repo.Commit(map[string]string{
		"elprovo.yml":        app,
		"proofs/feature.yml": "claim: The feature file says on.\nrun: echo feature is $(cat feature.txt); grep -qx on feature.txt\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.yml", replay.Failed, "fail fail", "fail fail", 1)
	head := r.proof(t, "proofs/feature.yml").Head[0]
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
	repo.Commit(map[string]string{"elprovo.yml": app, "proofs/feature.yml": featureProof + "before: skip\n"})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/feature.yml", replay.Held, "", "pass pass", 0)
	if !strings.Contains(r.stdout, "not compared with the base: before is skip\n") {
		t.Errorf("stdout does not say why the proof was not compared:\n%s", r.stdout)
	}
}

func TestAllReplaysUnchangedProofsOnTheHeadCommitOnly(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"elprovo.yml": app, "feature.txt": "on\n", "proofs/feature.yml": featureProof})
	repo.Commit(map[string]string{"README": "unrelated change\n"})

	r := replayIn(t, repo.Dir, "--base", base, "--all")

	wantResult(t, r, "proofs/feature.yml", replay.Held, "", "pass pass", 0)
	if r.proof(t, "proofs/feature.yml").Claimed {
		t.Error("the proof is claimed; the change does not touch it")
	}
}

func TestAChangeWithoutProofsSelectsNothing(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"elprovo.yml": app, "proofs/feature.yml": featureProof, "proofs/helper.sh": "true\n"})
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
		"elprovo.yml": app + "pass_env: [COUNTER]\n",
		// The proof passes on even counts and fails on odd ones.
		"proofs/alternate.yml": "claim: The proof alternates.\nbefore: skip\nrun: n=$(cat \"$COUNTER\"); echo $((n + 1)) > \"$COUNTER\"; [ $((n % 2)) -eq 0 ]\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	wantResult(t, r, "proofs/alternate.yml", replay.Flaky, "", "pass fail", 1)
}

func TestAnAppThatIsNeverReadyIsNotRun(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.yml":        "version: 1\nstart: \"true\"\nready: \"false\"\ntimeouts:\n  ready: 1s\n",
		"proofs/feature.yml": featureProof,
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.yml", replay.ResultNotRun, "not-run", "not-run", 2)
	if reason := r.proof(t, "proofs/feature.yml").Head[0].Reason; reason != "ready did not exit 0 within 1s" {
		t.Errorf("reason is %q; want %q", reason, "ready did not exit 0 within 1s")
	}
	if !strings.Contains(r.stdout, indent+"reason: ready did not exit 0 within 1s\n") {
		t.Errorf("stdout does not give the reason:\n%s", r.stdout)
	}
}

func TestAFailingStartIsNotRun(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.yml":        "version: 1\nstart: exit 4\nready: \"true\"\n",
		"proofs/feature.yml": featureProof + "before: skip\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/feature.yml", replay.ResultNotRun, "", "not-run", 2)
	if reason := r.proof(t, "proofs/feature.yml").Head[0].Reason; reason != "start exited with status 4" {
		t.Errorf("reason is %q; want %q", reason, "start exited with status 4")
	}
}

func TestATimedOutProofFailsAndItsProcessGroupEnds(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.yml":     app + "pass_env: [SPY]\n",
		"proofs/slow.yml": "claim: The proof is slow.\nbefore: skip\ntimeout: 1s\nrun: sleep 60 & echo $! > \"$SPY/pid\"; wait\n",
	})

	started := time.Now()
	r := replayIn(t, repo.Dir, "--base", base, "--runs", "1")

	wantResult(t, r, "proofs/slow.yml", replay.Failed, "", "fail", 1)
	head := r.proof(t, "proofs/slow.yml").Head[0]
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
		"elprovo.yml":  app + "stop: exit 3\n",
		"proofs/a.yml": "claim: A holds.\nbefore: skip\nrun: \"true\"\n",
		"proofs/b.yml": "claim: B holds.\nbefore: skip\nrun: \"true\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base)

	if r.status != 2 || r.report.StoppedEarly != "stop exited with status 3" {
		t.Errorf("exit %d, stopped_early %q; want 2 and %q\n%s", r.status, r.report.StoppedEarly, "stop exited with status 3", r.stdout)
	}
	if a := r.proof(t, "proofs/a.yml"); a.Result != replay.Unfinished || len(a.Head) != 1 {
		t.Errorf("proofs/a.yml has result %q and %d head runs; want no result and 1 run", a.Result, len(a.Head))
	}
	if b := r.proof(t, "proofs/b.yml"); len(b.Head) != 0 {
		t.Errorf("proofs/b.yml has %d head runs; want 0", len(b.Head))
	}
}

func TestAnInterruptRunsStopAndEndsTheReplay(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.yml":     app + "pass_env: [SPY]\nstop: touch \"$SPY/stopped\"\n",
		"proofs/slow.yml": "claim: The proof is slow.\nbefore: skip\nrun: sleep 60 & echo $! > \"$SPY/pid\"; wait\n",
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
	wantGone(t, filepath.Join(spy, "pid"))
}

func TestCommandsGetOnlyTheRunsEnvironmentAndAFreshCopy(t *testing.T) {
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	t.Setenv("PARENT_SECRET", "must not leak")
	base := repo.Commit(map[string]string{
		"src/proofs/keep.txt": "nested\n",
		"proofs/stale.txt":    "only in the base commit\n",
	})
	repo.Commit(map[string]string{
		"elprovo.yml": app + "env:\n  FROM_ENV: plain\npass_env: [SPY]\n",
		"proofs/spy.yml": "claim: The spy records what a run sees.\nrun: |-\n" +
			"  d=\"$SPY/$ELPROVO_RUN_ID\"; mkdir \"$d\"\n" +
			"  env | sort > \"$d/env\"; pwd > \"$d/pwd\"; ls -A \"$HOME\" > \"$d/home\"\n" +
			"  ls -A . proofs src/proofs > \"$d/files\"\n",
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
		{"unknown field", map[string]string{"elprovo.yml": app + "stopp: x\n", "proofs/feature.yml": featureProof}, nil, "field stopp not found"},
		{"invalid proof name", map[string]string{"elprovo.yml": app, "proofs/Feature.yml": featureProof}, nil, "proofs/Feature.yml: the name before .yml"},
		{"reserved pass_env name", map[string]string{"elprovo.yml": app + "pass_env: [ELPROVO_TEST_UNSET]\n", "proofs/feature.yml": featureProof}, nil, "must not start with ELPROVO_"},
		{"unset pass_env variable", map[string]string{"elprovo.yml": app + "pass_env: [UNSET_FOR_ELPROVO_TEST]\n", "proofs/feature.yml": featureProof}, nil, "pass_env names UNSET_FOR_ELPROVO_TEST, which is not set"},
		{"no elprovo.yml", map[string]string{"proofs/feature.yml": featureProof}, nil, "has no elprovo.yml"},
		{"unknown proof argument", map[string]string{"elprovo.yml": app, "proofs/feature.yml": featureProof}, []string{"proofs/missing.yml"}, "proofs/missing.yml is not a proof file"},
		{"too many runs", map[string]string{"elprovo.yml": app, "proofs/feature.yml": featureProof}, []string{"--runs", "11"}, "--runs is 11"},
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
		"elprovo.yml":        "version: 1\nstart: touch \"$SPY/started\"\nready: \"true\"\npass_env: [SPY]\n",
		"proofs/feature.yml": featureProof,
		"proofs/kept.yml":    "claim: Kept.\nbefore: skip\nrun: \"true\"\n",
	})

	r := replayIn(t, repo.Dir, "--base", base, "--dry-run")

	if r.status != 0 {
		t.Errorf("exit %d; want 0\n%s%s", r.status, r.stdout, r.stderr)
	}
	for _, line := range []string{"proofs/feature.yml  base and head", "proofs/kept.yml  head only: before is skip", "Dry run: no command ran."} {
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
