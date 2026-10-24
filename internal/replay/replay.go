// Package replay runs proofs on fresh copies of the base and head commits, as
// sections 3 to 5 of SPEC.md define.
package replay

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/elprovo/elprovo/internal/config"
	"github.com/elprovo/elprovo/internal/gitrepo"
)

const maxProofs = 100

// Limits bounds what a run keeps. A zero field takes the limit from SPEC.md.
type Limits struct {
	LogBytes       int64
	ArtifactsBytes int64
}

func (l Limits) withDefaults() Limits {
	if l.LogBytes == 0 {
		l.LogBytes = 10 << 20
	}
	if l.ArtifactsBytes == 0 {
		l.ArtifactsBytes = 1 << 30
	}
	return l
}

// Options are the inputs of a replay.
type Options struct {
	// Dir is a directory inside the repository.
	Dir string
	// Base and Head name the commits. An empty Base means the merge base of
	// the head commit and origin/HEAD. An empty Head means HEAD.
	Base, Head string
	All        bool
	Runs       int
	// Out is the output directory.
	Out string
	// Proofs, when not empty, are the only proof files to replay.
	Proofs []string
	// Environ is the environment of elprovo replay, as os.Environ gives it.
	Environ []string
	Limits  Limits
	// RunDone is called after each run ends. It may be nil.
	RunDone func(proof ProofReport, side string, run RunReport)
	// ProofDone is called after all the runs of a proof end. It may be nil.
	ProofDone func(ProofReport)
	// Warn reports a problem that does not change any result. It may be nil.
	Warn func(string)
}

// Planned is a selected proof.
type Planned struct {
	config.Proof
	// Claimed is true when the change adds or modifies the proof file.
	Claimed bool
	// OnBase is true when the proof runs on the base commit.
	OnBase bool
}

// Plan is a checked replay that has not run yet.
type Plan struct {
	Base, Head string
	Proofs     []Planned
	repo       gitrepo.Repo
	headConfig config.Repo
	// baseConfig is nil when the base commit has no elprovo.yml.
	baseConfig *config.Repo
}

// Prepare resolves the commits, selects the proofs and checks every file the
// replay needs, without running any command.
func Prepare(ctx context.Context, opts Options) (Plan, error) {
	if opts.Runs < 1 || opts.Runs > 10 {
		return Plan{}, fmt.Errorf("--runs is %d; it must be from 1 to 10", opts.Runs)
	}
	repo, err := gitrepo.Open(ctx, opts.Dir)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{repo: repo}
	if plan.Head, err = repo.Resolve(ctx, orDefault(opts.Head, "HEAD")); err != nil {
		return Plan{}, err
	}
	if plan.Base, err = resolveBase(ctx, repo, opts.Base, plan.Head); err != nil {
		return Plan{}, err
	}

	data, found, err := repo.ReadFile(ctx, plan.Head, config.RepoFile, config.MaxFileSize)
	if err != nil {
		return Plan{}, err
	}
	if !found {
		return Plan{}, fmt.Errorf("the head commit %s has no %s; add one, as the README shows", short(plan.Head), config.RepoFile)
	}
	if plan.headConfig, err = config.ParseRepo(config.RepoFile, data); err != nil {
		return Plan{}, err
	}

	changed, err := repo.AddedOrModified(ctx, plan.Base, plan.Head)
	if err != nil {
		return Plan{}, err
	}
	files, err := selectProofs(ctx, repo, plan.Head, changed, opts)
	if err != nil {
		return Plan{}, err
	}
	for _, file := range files {
		data, _, err := repo.ReadFile(ctx, plan.Head, file, config.MaxFileSize)
		if err != nil {
			return Plan{}, err
		}
		proof, err := config.ParseProof(file, data)
		if err != nil {
			return Plan{}, err
		}
		claimed := slices.Contains(changed, file)
		plan.Proofs = append(plan.Proofs, Planned{
			Proof:   proof,
			Claimed: claimed,
			OnBase:  claimed && proof.Before == config.BeforeFail,
		})
	}

	configs := []config.Repo{plan.headConfig}
	if slices.ContainsFunc(plan.Proofs, func(p Planned) bool { return p.OnBase }) {
		data, found, err := repo.ReadFile(ctx, plan.Base, config.RepoFile, config.MaxFileSize)
		if err != nil {
			return Plan{}, err
		}
		if found {
			base, err := config.ParseRepo(config.RepoFile+" in the base commit "+short(plan.Base), data)
			if err != nil {
				return Plan{}, err
			}
			plan.baseConfig = &base
			configs = append(configs, base)
		}
	}
	environ := envMap(opts.Environ)
	for _, c := range configs {
		for _, name := range c.PassEnv {
			if _, ok := environ[name]; !ok {
				return Plan{}, fmt.Errorf("pass_env names %s, which is not set in the environment of elprovo replay", name)
			}
		}
	}
	return plan, nil
}

func resolveBase(ctx context.Context, repo gitrepo.Repo, rev, head string) (string, error) {
	if rev != "" {
		return repo.Resolve(ctx, rev)
	}
	upstream, err := repo.Resolve(ctx, "origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("origin/HEAD does not exist, so give the base commit with --base, such as --base origin/main")
	}
	return repo.MergeBase(ctx, head, upstream)
}

// selectProofs applies section 3.1 of SPEC.md and returns the selected proof
// files in file name order.
func selectProofs(ctx context.Context, repo gitrepo.Repo, head string, changed []string, opts Options) ([]string, error) {
	var candidates []string
	switch {
	case len(opts.Proofs) > 0:
		inHead, err := repo.Files(ctx, head, config.ProofDir)
		if err != nil {
			return nil, err
		}
		for _, arg := range opts.Proofs {
			file, err := repoPath(repo.Root(), opts.Dir, arg)
			if err != nil {
				return nil, err
			}
			if _, ok, err := config.ProofName(file); err != nil || !ok || !slices.Contains(inHead, file) {
				return nil, fmt.Errorf("%q is not a proof file in the head commit %s", arg, short(head))
			}
			candidates = append(candidates, file)
		}
	case opts.All:
		inHead, err := repo.Files(ctx, head, config.ProofDir)
		if err != nil {
			return nil, err
		}
		candidates = inHead
	default:
		candidates = changed
	}

	var files []string
	for _, file := range candidates {
		_, ok, err := config.ProofName(file)
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, file)
		}
	}
	slices.Sort(files)
	files = slices.Compact(files)
	if len(files) > maxProofs {
		return nil, fmt.Errorf("%d proofs are selected; the limit is %d", len(files), maxProofs)
	}
	return files, nil
}

// repoPath turns arg, a path relative to the working directory dir, into a
// slash-separated path from the repository root. The file need not exist in
// the working tree, because Elprovo reads it from the head commit.
func repoPath(root, dir, arg string) (string, error) {
	path := arg
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	// Git reports the root with symbolic links resolved, so resolve the
	// directory that holds the file too.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(parent, filepath.Base(path))
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the repository %s", arg, root)
	}
	return filepath.ToSlash(rel), nil
}

// Run replays a prepared plan and writes report.json. It returns an error
// only when it cannot write the report. A replay that stops early returns a
// report whose StoppedEarly says why.
func Run(ctx context.Context, plan Plan, opts Options) (Report, error) {
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return Report{}, err
	}
	report := Report{Format: 1, Base: plan.Base, Head: plan.Head, Runs: opts.Runs}
	report.Dir = filepath.Join(out, "runs", randomID())
	if err := os.MkdirAll(report.Dir, 0o755); err != nil {
		return Report{}, err
	}
	r := runner{plan: plan, opts: opts, dir: report.Dir, limits: opts.Limits.withDefaults()}
	for _, p := range plan.Proofs {
		report.Proofs = append(report.Proofs, ProofReport{
			File: p.File, Claim: p.Claim, Before: p.Before, Claimed: p.Claimed,
			Result: Unfinished, Base: []RunReport{}, Head: []RunReport{},
		})
	}

	for i, p := range plan.Proofs {
		proofReport := &report.Proofs[i]
		stop := r.replayProof(ctx, p, proofReport)
		if len(proofReport.Head) == opts.Runs && (!p.OnBase || len(proofReport.Base) == opts.Runs) {
			proofReport.Result = resultOf(outcomes(proofReport.Head), outcomes(proofReport.Base))
			if opts.ProofDone != nil {
				opts.ProofDone(*proofReport)
			}
		}
		if stop != "" {
			report.StoppedEarly = stop
			break
		}
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	if err := os.WriteFile(filepath.Join(report.Dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return report, fmt.Errorf("write the report: %w", err)
	}
	return report, nil
}

func outcomes(runs []RunReport) []Outcome {
	var list []Outcome
	for _, run := range runs {
		list = append(list, run.Outcome)
	}
	return list
}

func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok {
			m[name] = value
		}
	}
	return m
}

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomID returns 8 random characters from [a-z0-9].
func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	for i := range b {
		b[i] = idChars[int(b[i])%len(idChars)]
	}
	return string(b)
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
