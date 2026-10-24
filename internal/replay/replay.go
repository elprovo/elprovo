// Package replay runs the tests of a change on fresh copies of the base and
// head commits, and gives each test a verdict, as sections 3 to 5 of SPEC.md
// define.
package replay

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/elprovo/elprovo-next/internal/config"
	"github.com/elprovo/elprovo-next/internal/gitrepo"
	"github.com/elprovo/elprovo-next/internal/gotest"
)

// maxTestFileSize is the largest test file that Elprovo reads to find the
// tests a change adds or edits.
const maxTestFileSize = 4 << 20

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
	Runs       int
	// Out is the output directory.
	Out string
	// Environ is the environment of elprovo replay, as os.Environ gives it.
	Environ []string
	Limits  Limits
	// RunDone is called after each run ends. It may be nil.
	RunDone func(side string, run RunReport)
	// Warn reports a problem that does not change any result. It may be nil.
	Warn func(string)
}

// ChangedTest is a test that the change adds or edits.
type ChangedTest struct {
	// Package is the package's directory, as go test takes it.
	Package string
	Name    string
	// File is the test's file, a slash-separated path from the repository
	// root.
	File string
}

// Plan is a checked replay that has not run yet.
type Plan struct {
	Base, Head string
	// Changed holds the tests that the change adds or edits, in file and
	// name order.
	Changed []ChangedTest
	// Packages are the directories of the Go files that the change adds,
	// modifies or deletes, in order, as go test takes them, such as ./cart.
	Packages []string
	repo     gitrepo.Repo
	// module is the path of the head commit's Go module.
	module     string
	headConfig config.Repo
	// baseConfig is nil when the base commit has no elprovo.toml of version
	// 2. The base runs then use the head commit's.
	baseConfig *config.Repo
	// reproduce is true for a plan of elprovo reproduce, which runs on the
	// base commit only.
	reproduce bool
}

// Prepare resolves the commits, finds the changed tests and the packages to
// test, and checks every file the replay needs, without running any command.
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

	files, err := repo.ChangedFiles(ctx, plan.Base, plan.Head)
	if err != nil {
		return Plan{}, err
	}
	for _, file := range files {
		dir, ok := packageDir(file)
		if !ok {
			continue
		}
		if !slices.Contains(plan.Packages, dir) {
			plan.Packages = append(plan.Packages, dir)
		}
		if !strings.HasSuffix(file, "_test.go") || !plan.headConfig.Tests.Matches(file) {
			continue
		}
		names, err := changedTests(ctx, repo, plan.Base, plan.Head, file)
		if err != nil {
			return Plan{}, err
		}
		for _, name := range names {
			plan.Changed = append(plan.Changed, ChangedTest{Package: dir, Name: name, File: file})
		}
	}
	slices.Sort(plan.Packages)
	if len(plan.Packages) > 0 {
		if plan.module, err = modulePath(ctx, repo, plan.Head); err != nil {
			return Plan{}, err
		}
	}

	configs := []config.Repo{plan.headConfig}
	data, found, err = repo.ReadFile(ctx, plan.Base, config.RepoFile, config.MaxFileSize)
	if err != nil {
		return Plan{}, err
	}
	if found {
		base, err := config.ParseRepo(config.RepoFile+" in the base commit "+short(plan.Base), data)
		switch {
		case errors.Is(err, config.ErrVersion1):
		case err != nil:
			return Plan{}, err
		default:
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

// PrepareReproduce is Prepare for elprovo reproduce. The plan runs on the
// base commit only, and gives a verdict to the changed tests in files, which
// are paths relative to opts.Dir.
func PrepareReproduce(ctx context.Context, opts Options, files []string) (Plan, error) {
	if len(files) == 0 {
		return Plan{}, errors.New("reproduce needs at least one test file, such as cart/cart_test.go")
	}
	plan, err := Prepare(ctx, opts)
	if err != nil {
		return Plan{}, err
	}
	var paths []string
	for _, arg := range files {
		file, err := repoPath(plan.repo.Root(), opts.Dir, arg)
		if err != nil {
			return Plan{}, err
		}
		if !strings.HasSuffix(file, "_test.go") || !plan.headConfig.Tests.Matches(file) {
			return Plan{}, fmt.Errorf("%q is not a test file: it does not match tests.files in %s, or its name does not end in _test.go", arg, config.RepoFile)
		}
		if _, ok := packageDir(file); !ok {
			return Plan{}, fmt.Errorf("%q is in a directory that go test leaves out, such as testdata", arg)
		}
		_, found, err := plan.repo.ReadFile(ctx, plan.Head, file, maxTestFileSize)
		if err != nil {
			return Plan{}, err
		}
		if !found {
			return Plan{}, fmt.Errorf("%q is not in the head commit %s; commit it first", arg, short(plan.Head))
		}
		if !slices.ContainsFunc(plan.Changed, func(t ChangedTest) bool { return t.File == file }) {
			return Plan{}, fmt.Errorf("%q has no test that the head commit %s adds or edits, compared with the base commit %s", arg, short(plan.Head), short(plan.Base))
		}
		paths = append(paths, file)
	}

	var changed []ChangedTest
	var packages []string
	for _, t := range plan.Changed {
		if !slices.Contains(paths, t.File) {
			continue
		}
		changed = append(changed, t)
		if !slices.Contains(packages, t.Package) {
			packages = append(packages, t.Package)
		}
	}
	slices.Sort(packages)
	plan.Changed, plan.Packages, plan.reproduce = changed, packages, true
	return plan, nil
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

// packageDir returns the directory of a Go file as go test takes it, such as
// ./cart, or false when file is not a Go file or go test leaves it out: a
// file in testdata or vendor, or one whose path has a name that starts with
// . or _.
func packageDir(file string) (string, bool) {
	if !strings.HasSuffix(file, ".go") {
		return "", false
	}
	for _, name := range strings.Split(file, "/") {
		if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return "", false
		}
	}
	if dir := path.Dir(file); dir != "." {
		return "./" + dir, true
	}
	return ".", true
}

// changedTests returns the tests in file that the head commit adds or
// declares differently from the base commit. It returns none when the head
// commit deletes the file.
func changedTests(ctx context.Context, repo gitrepo.Repo, base, head, file string) ([]string, error) {
	headData, found, err := repo.ReadFile(ctx, head, file, maxTestFileSize)
	if err != nil || !found {
		return nil, err
	}
	baseData, _, err := repo.ReadFile(ctx, base, file, maxTestFileSize)
	if err != nil {
		return nil, err
	}
	names, err := gotest.Changed(baseData, headData)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid Go in the base or the head commit: %v", config.EscapeControl(file), config.EscapeControl(err.Error()))
	}
	return names, nil
}

// modulePath reads the module path from the go.mod at the root of commit.
func modulePath(ctx context.Context, repo gitrepo.Repo, commit string) (string, error) {
	data, found, err := repo.ReadFile(ctx, commit, "go.mod", config.MaxFileSize)
	if err != nil {
		return "", err
	}
	if found {
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "module" {
				return strings.Trim(fields[1], "\"`"), nil
			}
		}
	}
	return "", fmt.Errorf("the head commit %s has no go.mod with a module line at the repository root; this release tests one Go module, at the root", short(commit))
}

// importPath returns the import path of pkg, a directory such as ./cart, as
// go test prints it.
func (p Plan) importPath(pkg string) string {
	return p.module + strings.TrimPrefix(pkg, ".")
}

// Run replays a prepared plan and writes report.json. It returns an error
// only when it cannot write the report. A replay that stops early returns a
// report whose StoppedEarly says why.
func Run(ctx context.Context, plan Plan, opts Options) (Report, error) {
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		Format: 2, Command: CommandReplay, Base: plan.Base, Head: plan.Head, Runs: opts.Runs, Packages: append([]string{}, plan.Packages...),
		Changed: []TestReport{}, Regressions: []TestReport{}, BaseRuns: []RunReport{}, HeadRuns: []RunReport{},
	}
	report.Dir = filepath.Join(out, "runs", randomID())
	if err := os.MkdirAll(report.Dir, 0o755); err != nil {
		return Report{}, err
	}
	r := runner{plan: plan, opts: opts, dir: report.Dir, limits: opts.Limits.withDefaults()}
	sides := []string{"base", "head"}
	if plan.reproduce {
		report.Command = CommandReproduce
		sides = sides[:1]
	}

	var baseResults, headResults []gotest.Results
runs:
	for n := 1; n <= opts.Runs; n++ {
		for _, side := range sides {
			if ctx.Err() != nil {
				report.StoppedEarly = "interrupted"
				break runs
			}
			run, results, stop := r.runOnce(ctx, side, n)
			if run != nil {
				if side == "base" {
					report.BaseRuns = append(report.BaseRuns, *run)
					baseResults = append(baseResults, results)
				} else {
					report.HeadRuns = append(report.HeadRuns, *run)
					headResults = append(headResults, results)
				}
				if opts.RunDone != nil {
					opts.RunDone(side, *run)
				}
				if run.Outcome == RunNotRun && stop == "" {
					stop = fmt.Sprintf("%s run %d did not run: %s", side, n, run.Reason)
				}
			}
			if stop != "" {
				report.StoppedEarly = stop
				break runs
			}
		}
	}
	report.Changed, report.Regressions = plan.testReports(baseResults, headResults, report.StoppedEarly == "")

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	if err := os.WriteFile(filepath.Join(report.Dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return report, fmt.Errorf("write the report: %w", err)
	}
	return report, nil
}

// testReports gives each changed test, and each other test that a run
// printed a result for, its results and, when every run ended, its verdict.
func (p Plan) testReports(base, head []gotest.Results, finished bool) (changed, regressions []TestReport) {
	changed, regressions = []TestReport{}, []TestReport{}
	isChanged := map[gotest.Test]bool{}
	for _, c := range p.Changed {
		t := gotest.Test{Package: p.importPath(c.Package), Name: c.Name}
		isChanged[t] = true
		report := TestReport{Package: c.Package, Name: c.Name, File: c.File, Base: testRuns(base, t), Head: testRuns(head, t)}
		switch {
		case !finished:
		case p.reproduce:
			report.Verdict = reproduceVerdict(outcomes(report.Base))
		default:
			report.Verdict = changedVerdict(outcomes(report.Head), outcomes(report.Base))
		}
		changed = append(changed, report)
	}
	if p.reproduce {
		return changed, regressions
	}

	pkgOf := map[string]string{}
	for _, pkg := range p.Packages {
		pkgOf[p.importPath(pkg)] = pkg
	}
	seen := map[gotest.Test]bool{}
	var others []gotest.Test
	for _, results := range slices.Concat(base, head) {
		for t := range results.Tests {
			if _, ok := pkgOf[t.Package]; ok && !isChanged[t] && !seen[t] {
				seen[t] = true
				others = append(others, t)
			}
		}
	}
	slices.SortFunc(others, func(a, b gotest.Test) int {
		return strings.Compare(pkgOf[a.Package]+"\x00"+a.Name, pkgOf[b.Package]+"\x00"+b.Name)
	})
	for _, t := range others {
		report := TestReport{Package: pkgOf[t.Package], Name: config.EscapeControl(t.Name), Base: testRuns(base, t), Head: testRuns(head, t)}
		if finished {
			report.Verdict = regressionVerdict(outcomes(report.Head), outcomes(report.Base))
		}
		regressions = append(regressions, report)
	}
	return changed, regressions
}

// testRuns returns the result of t in each run, with control characters in
// the messages escaped.
func testRuns(runs []gotest.Results, t gotest.Test) []TestRun {
	list := []TestRun{}
	for _, results := range runs {
		result := results.Result(t)
		message := result.Message
		if result.Outcome == gotest.Absent && message == "" {
			message = "the run printed no result for this test"
		}
		list = append(list, TestRun{Outcome: result.Outcome, Message: config.EscapeControl(message)})
	}
	return list
}

func outcomes(runs []TestRun) []gotest.Outcome {
	var list []gotest.Outcome
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
