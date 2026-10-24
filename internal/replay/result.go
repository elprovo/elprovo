package replay

import (
	"slices"

	"github.com/elprovo/elprovo-next/internal/gotest"
)

// RunOutcome is how one run on one commit ended.
type RunOutcome string

const (
	// Ran means the tests ran and printed their results.
	Ran RunOutcome = "ran"
	// RunNotRun means the app did not start, or the tests printed no
	// results at all. The replay stops after such a run.
	RunNotRun RunOutcome = "not-run"
)

// Verdict is what all the runs show about one test, from section 3.4 of
// SPEC.md.
type Verdict string

const (
	// The verdicts of a changed test.
	Fixed         Verdict = "fixed"
	NotReproduced Verdict = "not-reproduced"
	New           Verdict = "new"
	NotFixed      Verdict = "not-fixed"
	// The verdicts of another test.
	Kept           Verdict = "kept"
	Broke          Verdict = "broke"
	AlreadyFailing Verdict = "already-failing"
	NotRun         Verdict = "not-run"
	// The verdicts of a test that elprovo reproduce runs.
	Reproduced  Verdict = "reproduced"
	CouldNotRun Verdict = "could-not-run"
	// The verdicts of every kind of test. An unchanged test that fails
	// without the change and passes with it is fixed too.
	Flaky   Verdict = "flaky"
	Skipped Verdict = "skipped"
	// Unfinished is the verdict of every test when the replay stopped early.
	Unfinished Verdict = ""
)

// RunReport is one run on one commit.
type RunReport struct {
	Run     int        `json:"run"`
	Outcome RunOutcome `json:"outcome"`
	// ExitCode is the exit status of the test command, which decides
	// nothing.
	ExitCode         *int    `json:"exit_code"`
	TimedOut         bool    `json:"timed_out"`
	Seconds          float64 `json:"seconds"`
	Reason           string  `json:"reason"`
	LastLine         string  `json:"last_line"`
	ArtifactsDropped bool    `json:"artifacts_dropped"`
	Dir              string  `json:"dir"`
	// Log is the log of the command that decided the outcome, relative to
	// the replay's directory: the test command's, or for a run that did not
	// run, the log of start, ready or the test command. It is empty when no
	// such command ran.
	Log string `json:"log"`
	// LogDir is the absolute path of the run's directory. It is not in the
	// report, which uses paths relative to the replay's directory.
	LogDir string `json:"-"`
}

// TestRun is a test's result in one run.
type TestRun struct {
	Outcome gotest.Outcome `json:"outcome"`
	Message string         `json:"message"`
	// stopped is true for an absent test whose package failed in the run,
	// such as by a build error, a panic or a timeout.
	stopped bool
}

// stopped is the outcome that regressionVerdict gets for a stopped run.
const stopped gotest.Outcome = "stopped"

// TestReport is one test, its result in every run, and its verdict.
type TestReport struct {
	// Package is the package's directory, as go test takes it, such as
	// ./cart.
	Package string `json:"package"`
	Name    string `json:"name"`
	// File is the test's file. It is empty for a test that the change does
	// not add or edit.
	File    string    `json:"file,omitempty"`
	Verdict Verdict   `json:"verdict"`
	Base    []TestRun `json:"base"`
	Head    []TestRun `json:"head"`
}

// The commands that write a report.
const (
	CommandReplay    = "replay"
	CommandReproduce = "reproduce"
)

// Report is the result of one replay, as report.json holds it.
type Report struct {
	Format int `json:"format"`
	// Command is CommandReplay or CommandReproduce. A reproduce report has
	// base runs only, and no other tests.
	Command      string `json:"command"`
	Base         string `json:"base"`
	Head         string `json:"head"`
	Runs         int    `json:"runs"`
	StoppedEarly string `json:"stopped_early"`
	// Packages are the packages that the runs test.
	Packages    []string     `json:"packages"`
	Changed     []TestReport `json:"changed"`
	Regressions []TestReport `json:"regressions"`
	BaseRuns    []RunReport  `json:"base_runs"`
	HeadRuns    []RunReport  `json:"head_runs"`
	// Dir is the replay's output directory.
	Dir string `json:"-"`
}

// changedVerdict applies the rules for a changed test, in order.
func changedVerdict(head, base []gotest.Outcome) Verdict {
	switch {
	case anyOf(head, gotest.Failed, gotest.Absent) && anyOf(head, gotest.Passed):
		return Flaky
	case anyOf(head, gotest.Failed, gotest.Absent):
		return NotFixed
	case all(head, gotest.Skipped) || all(base, gotest.Skipped):
		return Skipped
	case anyOf(head, gotest.Skipped):
		return Flaky
	case all(base, gotest.Failed):
		return Fixed
	case all(base, gotest.Passed):
		return NotReproduced
	case all(base, gotest.Absent):
		return New
	default:
		return Flaky
	}
}

// reproduceVerdict applies the rules for a test that elprovo reproduce
// runs, in order.
func reproduceVerdict(base []gotest.Outcome) Verdict {
	switch {
	case all(base, gotest.Failed):
		return Reproduced
	case all(base, gotest.Passed):
		return NotReproduced
	case all(base, gotest.Absent):
		return CouldNotRun
	case all(base, gotest.Skipped):
		return Skipped
	default:
		return Flaky
	}
}

// regressionVerdict applies the rules for a test that the change does not
// add or edit, in order. Both commits run the same test files, so a test that
// passed without the change and was stopped with it was stopped by the
// change, such as by a build error or a panic. An absent test that was not
// stopped is not in the commit.
func regressionVerdict(head, base []gotest.Outcome) Verdict {
	switch {
	case anyOf(head, gotest.Failed) && !anyOf(base, gotest.Failed) && anyOf(head, gotest.Passed):
		return Flaky
	case anyOf(head, gotest.Failed) && !anyOf(base, gotest.Failed):
		return Broke
	case anyOf(base, gotest.Absent, stopped):
		return NotRun
	case anyOf(head, stopped) && all(base, gotest.Passed):
		return Broke
	case anyOf(head, gotest.Absent, stopped):
		return NotRun
	case !all(head, head[0]) || !all(base, base[0]):
		return Flaky
	case base[0] == gotest.Skipped || head[0] == gotest.Skipped:
		return Skipped
	case base[0] == gotest.Passed && head[0] == gotest.Passed:
		return Kept
	case base[0] == gotest.Passed:
		return Broke
	case head[0] == gotest.Failed:
		return AlreadyFailing
	default:
		return Fixed
	}
}

func anyOf(outcomes []gotest.Outcome, want ...gotest.Outcome) bool {
	return slices.ContainsFunc(outcomes, func(o gotest.Outcome) bool { return slices.Contains(want, o) })
}

func all(outcomes []gotest.Outcome, want gotest.Outcome) bool {
	return !slices.ContainsFunc(outcomes, func(o gotest.Outcome) bool { return o != want })
}

// ExitStatus is the exit status of section 3.5 of SPEC.md for a replay that
// ran. A replay that could not start exits 2 without a report.
func (r Report) ExitStatus(require bool) int {
	if r.StoppedEarly != "" {
		return 2
	}
	if r.Command == CommandReproduce {
		status := 0
		for _, t := range r.Changed {
			switch t.Verdict {
			case CouldNotRun:
				return 2
			case NotReproduced, Flaky, Skipped:
				status = 1
			}
		}
		return status
	}
	for _, t := range slices.Concat(r.Changed, r.Regressions) {
		switch t.Verdict {
		case NotFixed, NotReproduced, Flaky, Broke:
			return 1
		}
	}
	if require && !r.HasFixed() {
		return 1
	}
	return 0
}

// HasFixed reports whether any test is fixed.
func (r Report) HasFixed() bool {
	return slices.ContainsFunc(slices.Concat(r.Changed, r.Regressions), func(t TestReport) bool { return t.Verdict == Fixed })
}
