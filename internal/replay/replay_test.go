package replay_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elprovo/elprovo-next/internal/replay"
	"github.com/elprovo/elprovo-next/internal/testrepo"
)

// replayOnce prepares and runs a replay with one run on each commit.
func replayOnce(t *testing.T, repo testrepo.Repo, base string, limits replay.Limits) (replay.Report, []string) {
	t.Helper()
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
	repo := testrepo.New(t)
	spy := t.TempDir()
	t.Setenv("SPY", spy)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml": "version = 1\nstart = \"true\"\nready = \"true\"\npass_env = [\"SPY\"]\n",
		// The proof prints 1000 bytes, writes 2000 bytes of artifacts, and
		// leaves a read-only directory in its home, as the Go module cache does.
		"proofs/big.toml": "claim = \"The proof writes a lot.\"\nbefore = \"skip\"\nrun = '''\n" +
			"head -c 1000 /dev/zero | tr '\\0' a\n" +
			"head -c 2000 /dev/zero > \"$ELPROVO_ARTIFACTS/blob\"\n" +
			"mkdir -p \"$HOME/cache/module\" && touch \"$HOME/cache/module/file\" && chmod 555 \"$HOME/cache/module\" \"$HOME/cache\"\n" +
			"pwd > \"$SPY/workdir\"\n'''\n",
		// The small proof's artifacts are under the limit, so they are kept.
		"proofs/small.toml": "claim = \"The proof writes a little.\"\nbefore = \"skip\"\nrun = 'echo kept > \"$ELPROVO_ARTIFACTS/note.txt\"'\n",
	})

	report, warnings := replayOnce(t, repo, base, replay.Limits{LogBytes: 100, ArtifactsBytes: 1000})

	big := report.Proofs[0].Head[0]
	if big.Outcome != replay.Pass || !big.ArtifactsDropped {
		t.Errorf("outcome %q, artifacts_dropped %v; want pass and true", big.Outcome, big.ArtifactsDropped)
	}
	if entries, _ := os.ReadDir(filepath.Join(big.LogDir, "artifacts")); len(entries) != 0 {
		t.Errorf("the artifacts directory holds %d entries after the drop; want 0", len(entries))
	}
	log, err := os.ReadFile(filepath.Join(big.LogDir, "proof.log"))
	if err != nil {
		t.Fatal(err)
	}
	wantLog := strings.Repeat("a", 100) + "\n[elprovo: this log was cut to its first 100 bytes]\n"
	if string(log) != wantLog {
		t.Errorf("proof.log is %q; want %q", log, wantLog)
	}

	small := report.Proofs[1].Head[0]
	if note, err := os.ReadFile(filepath.Join(small.LogDir, "artifacts", "note.txt")); err != nil || string(note) != "kept\n" || small.ArtifactsDropped {
		t.Errorf("the small proof's artifact is %q (%v), artifacts_dropped %v; want \"kept\\n\" and false", note, err, small.ArtifactsDropped)
	}

	if len(warnings) > 0 {
		t.Errorf("warnings: %v; want none", warnings)
	}
	workdir, err := os.ReadFile(filepath.Join(spy, "workdir"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(strings.TrimSpace(string(workdir)))); !os.IsNotExist(err) {
		t.Errorf("the run's temporary directory is still there after the run: %v", err)
	}
}

func TestUnreadableArtifactsAreDroppedWithAWarning(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.toml": "version = 1\nstart = \"true\"\nready = \"true\"\n",
		// The artifacts are under the size limit; only the locked directory
		// stops Elprovo from measuring them.
		"proofs/locked.toml": "claim = \"The proof locks a directory.\"\nbefore = \"skip\"\nrun = '''\n" +
			"echo small > \"$ELPROVO_ARTIFACTS/note.txt\"\n" +
			"mkdir \"$ELPROVO_ARTIFACTS/locked\" && chmod 000 \"$ELPROVO_ARTIFACTS/locked\"\n'''\n",
	})

	report, warnings := replayOnce(t, repo, base, replay.Limits{ArtifactsBytes: 1000})

	run := report.Proofs[0].Head[0]
	if !run.ArtifactsDropped || len(warnings) != 1 || !strings.Contains(warnings[0], "could not measure the artifacts") {
		t.Errorf("artifacts_dropped %v, warnings %q; want true and one warning about measuring the artifacts", run.ArtifactsDropped, warnings)
	}
}
