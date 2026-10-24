package replay_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/elprovo/elprovo-next/internal/replay"
	"github.com/elprovo/elprovo-next/internal/testrepo"
)

// scriptTests is an elprovo.toml whose test command is run.sh in the copy.
const scriptTests = "version = 2\npass_env = [\"SPY\"]\n\n[tests]\nfiles = [\"**/*_test.go\"]\nrun = \"sh run.sh {packages}\"\nreport = \"go-test-json\"\n"

// passEvent is a go test -json event that makes a run count as one that ran.
const passEvent = `{"Action":"pass","Package":"example.com/m"}`

// replayOnce makes a change to a Go package whose tests run.sh runs, and
// replays it with one run on each commit.
func replayOnce(t *testing.T, script string, limits replay.Limits) (replay.Report, []string) {
	t.Helper()
	t.Setenv("SPY", t.TempDir())
	repo := testrepo.New(t)
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptTests, "run.sh": script, "a.go": "package m\n"}))
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n"})

	var warnings []string
	opts := replay.Options{
		Dir: repo.Dir, Base: base, Runs: 1, Out: filepath.Join(repo.Dir, ".elprovo"),
		Environ: os.Environ(),
		Limits:  limits,
		Warn:    func(m string) { warnings = append(warnings, m) },
	}
	plan, err := replay.Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	report, err := replay.Run(context.Background(), plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	return report, warnings
}

func TestARunKeepsWithinItsLimitsAndLeavesNothingBehind(t *testing.T) {
	// The tests print 1000 bytes, write 2000 bytes of artifacts, and leave a
	// read-only directory in their home, as the Go module cache does.
	report, warnings := replayOnce(t, "echo '"+passEvent+"'\n"+
		"head -c 1000 /dev/zero | tr '\\0' a\n"+
		"head -c 2000 /dev/zero > \"$ELPROVO_ARTIFACTS/blob\"\n"+
		"mkdir -p \"$HOME/cache/module\" && touch \"$HOME/cache/module/file\" && chmod 555 \"$HOME/cache/module\" \"$HOME/cache\"\n"+
		"pwd > \"$SPY/workdir\"\n", replay.Limits{LogBytes: 100, ArtifactsBytes: 1000})

	run := report.HeadRuns[0]
	if run.Outcome != replay.Ran || !run.ArtifactsDropped {
		t.Errorf("outcome %q, artifacts_dropped %v; want ran and true", run.Outcome, run.ArtifactsDropped)
	}
	if entries, _ := os.ReadDir(filepath.Join(run.LogDir, "artifacts")); len(entries) != 0 {
		t.Errorf("the artifacts directory holds %d entries after the drop; want 0", len(entries))
	}
	log, err := os.ReadFile(filepath.Join(run.LogDir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	wantLog := passEvent + "\n" + strings.Repeat("a", 100-len(passEvent)-1) + "\n[elprovo: this log was cut to its first 100 bytes]\n"
	if string(log) != wantLog {
		t.Errorf("test.log is %q; want %q", log, wantLog)
	}
	if len(warnings) > 0 {
		t.Errorf("warnings: %v; want none", warnings)
	}
	workdir, err := os.ReadFile(filepath.Join(os.Getenv("SPY"), "workdir"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(strings.TrimSpace(string(workdir)))); !os.IsNotExist(err) {
		t.Errorf("the run's temporary directory is still there after the run: %v", err)
	}
}

func TestArtifactsUnderTheLimitAreKept(t *testing.T) {
	report, _ := replayOnce(t, "echo '"+passEvent+"'\necho kept > \"$ELPROVO_ARTIFACTS/note.txt\"\n", replay.Limits{ArtifactsBytes: 1000})

	run := report.HeadRuns[0]
	if note, err := os.ReadFile(filepath.Join(run.LogDir, "artifacts", "note.txt")); err != nil || string(note) != "kept\n" || run.ArtifactsDropped {
		t.Errorf("the artifact is %q (%v), artifacts_dropped %v; want \"kept\\n\" and false", note, err, run.ArtifactsDropped)
	}
}

func TestUnreadableArtifactsAreDroppedWithAWarning(t *testing.T) {
	// The artifacts are under the size limit; only the locked directory
	// stops Elprovo from measuring them.
	report, warnings := replayOnce(t, "echo '"+passEvent+"'\n"+
		"echo small > \"$ELPROVO_ARTIFACTS/note.txt\"\n"+
		"mkdir \"$ELPROVO_ARTIFACTS/locked\" && chmod 000 \"$ELPROVO_ARTIFACTS/locked\"\n", replay.Limits{ArtifactsBytes: 1000})

	run := report.BaseRuns[0]
	if !run.ArtifactsDropped || len(warnings) != 2 || !strings.Contains(warnings[0], "could not measure the artifacts") {
		t.Errorf("artifacts_dropped %v, warnings %q; want true and a warning about measuring the artifacts of each run", run.ArtifactsDropped, warnings)
	}
}

func TestTheLastLineIsReadBeforeTheLogIsCut(t *testing.T) {
	report, _ := replayOnce(t, "yes a | head -c 1000; echo the end\n", replay.Limits{LogBytes: 100})

	run := report.BaseRuns[0]
	if run.Outcome != replay.RunNotRun || run.LastLine != "the end" {
		t.Errorf("outcome %q, last_line %q; want not-run and %q", run.Outcome, run.LastLine, "the end")
	}
}

func TestTheLastLineKeepsAtMost200Characters(t *testing.T) {
	report, _ := replayOnce(t, "printf 'é%.0s' $(seq 300); echo\n", replay.Limits{})

	if got := report.BaseRuns[0].LastLine; got != strings.Repeat("é", 200) {
		t.Errorf("last_line has %d characters in %d bytes; want 200 é characters", utf8.RuneCountInString(got), len(got))
	}
}

func TestPackagesWithUnusualNamesReachTheTestCommandAsOneWordEach(t *testing.T) {
	t.Setenv("SPY", t.TempDir())
	repo := testrepo.New(t)
	script := "printf '%s\\n' \"$@\" > \"$SPY/args\"\necho '" + passEvent + "'\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptTests, "run.sh": script}))
	repo.Commit(map[string]string{"a.go": "package m\n", "it's $(here)/b.go": "package here\n"})

	opts := replay.Options{Dir: repo.Dir, Base: base, Runs: 1, Out: filepath.Join(repo.Dir, ".elprovo"), Environ: os.Environ()}
	plan, err := replay.Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Run(context.Background(), plan, opts); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(filepath.Join(os.Getenv("SPY"), "args"))
	if want := ".\n./it's $(here)\n"; err != nil || string(args) != want {
		t.Errorf("run.sh got %q (%v); want %q", args, err, want)
	}
}

func TestARunTestsOnlyThePackagesItsCommitHas(t *testing.T) {
	t.Setenv("SPY", t.TempDir())
	repo := testrepo.New(t)
	script := "echo \"$*\" >> \"$SPY/args\"\necho '" + passEvent + "'\n"
	base := repo.Commit(testrepo.GoModule(map[string]string{"elprovo.toml": scriptTests, "run.sh": script, "a.go": "package m\n"}))
	repo.Commit(map[string]string{"a.go": "package m\n\nconst A = 1\n", "added/b.go": "package added\n"})

	opts := replay.Options{Dir: repo.Dir, Base: base, Runs: 1, Out: filepath.Join(repo.Dir, ".elprovo"), Environ: os.Environ()}
	plan, err := replay.Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Run(context.Background(), plan, opts); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(filepath.Join(os.Getenv("SPY"), "args"))
	if want := ".\n. ./added\n"; err != nil || string(args) != want {
		t.Errorf("the base run and the head run got the packages %q (%v); want %q", args, err, want)
	}
}
