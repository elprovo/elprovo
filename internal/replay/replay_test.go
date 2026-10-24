package replay_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elprovo/elprovo/internal/replay"
	"github.com/elprovo/elprovo/internal/testrepo"
)

func TestARunKeepsWithinItsLimitsAndLeavesNothingBehind(t *testing.T) {
	repo := testrepo.New(t)
	base := repo.Commit(map[string]string{"README": "base\n"})
	repo.Commit(map[string]string{
		"elprovo.yml": "version: 1\nstart: \"true\"\nready: \"true\"\n",
		// The proof prints 1000 bytes, writes 2000 bytes of artifacts, and
		// leaves a read-only directory in its home, as the Go module cache does.
		"proofs/big.yml": "claim: The proof writes a lot.\nbefore: skip\nrun: |-\n" +
			"  head -c 1000 /dev/zero | tr '\\0' a\n" +
			"  head -c 2000 /dev/zero > \"$ELPROVO_ARTIFACTS/blob\"\n" +
			"  mkdir -p \"$HOME/cache/module\" && touch \"$HOME/cache/module/file\" && chmod 555 \"$HOME/cache/module\" \"$HOME/cache\"\n" +
			"  pwd > \"$ELPROVO_ARTIFACTS/../workdir\"\n",
	})

	var warnings []string
	opts := replay.Options{
		Dir: repo.Dir, Base: base, Runs: 1, Out: filepath.Join(repo.Dir, ".elprovo"),
		Environ: os.Environ(),
		Limits:  replay.Limits{LogBytes: 100, ArtifactsBytes: 1000},
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

	run := report.Proofs[0].Head[0]
	if run.Outcome != replay.Pass || !run.ArtifactsDropped {
		t.Errorf("outcome %q, artifacts_dropped %v; want pass and true", run.Outcome, run.ArtifactsDropped)
	}
	if entries, _ := os.ReadDir(filepath.Join(run.LogDir, "artifacts")); len(entries) != 0 {
		t.Errorf("the artifacts directory holds %d entries after the drop; want 0", len(entries))
	}
	log, err := os.ReadFile(filepath.Join(run.LogDir, "proof.log"))
	if err != nil {
		t.Fatal(err)
	}
	wantLog := strings.Repeat("a", 100) + "\n[elprovo: this log was cut to its first 100 bytes]\n"
	if string(log) != wantLog {
		t.Errorf("proof.log is %q; want %q", log, wantLog)
	}

	if len(warnings) > 0 {
		t.Errorf("warnings: %v; want none", warnings)
	}
	workdir, err := os.ReadFile(filepath.Join(run.LogDir, "workdir"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(strings.TrimSpace(string(workdir)))); !os.IsNotExist(err) {
		t.Errorf("the run's temporary directory is still there after the run: %v", err)
	}
}
