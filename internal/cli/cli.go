// Package cli is the elprovo command: its arguments, its text output and its
// exit status.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/elprovo/elprovo-next/internal/config"
	"github.com/elprovo/elprovo-next/internal/gotest"
	"github.com/elprovo/elprovo-next/internal/replay"
)

// Env is what the command gets from the process that runs it.
type Env struct {
	Stdout, Stderr io.Writer
	// Environ is the process environment, as os.Environ gives it.
	Environ []string
	// Dir is the working directory.
	Dir string
}

const usage = `Elprovo runs the tests of a change on fresh copies of the code without the
change and with it, and shows which tests the change fixed and which it broke.

Usage:
  elprovo replay [options]   compare the tests; see elprovo replay -h
  elprovo version            print the version

SPEC.md in the Elprovo repository defines the files and the results.
`

// Run runs the command with args, the arguments after the program name, and
// returns its exit status.
func Run(ctx context.Context, version string, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "replay":
		return runReplay(ctx, args[1:], env)
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "elprovo %s\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return 0
	default:
		fmt.Fprintf(env.Stderr, "elprovo: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runReplay(ctx context.Context, args []string, env Env) int {
	flags := flag.NewFlagSet("elprovo replay", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	opts := replay.Options{Dir: env.Dir, Environ: env.Environ}
	flags.StringVar(&opts.Base, "base", "", "the commit without the change (default: the merge base of the head commit and origin/HEAD)")
	flags.StringVar(&opts.Head, "head", "", "the commit with the change (default: HEAD)")
	flags.IntVar(&opts.Runs, "runs", 2, "the number of runs on each commit, from 1 to 10")
	require := flags.Bool("require", false, "treat a replay in which no test is fixed as a failure")
	flags.StringVar(&opts.Out, "out", ".elprovo", "the directory for logs, artifacts and the report")
	dryRun := flags.Bool("dry-run", false, "check the files and print the changed tests, without running any command")
	flags.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: elprovo replay [options]\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "elprovo: replay takes no arguments, but got %q; it finds the changed tests itself\n", config.EscapeControl(flags.Arg(0)))
		return 2
	}
	if !filepath.IsAbs(opts.Out) {
		opts.Out = filepath.Join(env.Dir, opts.Out)
	}

	plan, err := replay.Prepare(ctx, opts)
	if err != nil {
		fmt.Fprintf(env.Stderr, "elprovo: %v\n", err)
		return 2
	}
	out := output{w: env.Stdout, dir: env.Dir}
	out.header(plan, opts.Runs, *dryRun)
	if len(plan.Packages) == 0 {
		fmt.Fprintln(out.w, "The change touches no Go package, so there is no test to run.")
		if *require {
			fmt.Fprintln(out.w, "--require is on, so this is a failure.")
			return 1
		}
		return 0
	}
	if *dryRun {
		out.dryRun(plan)
		return 0
	}

	opts.RunDone = func(side string, run replay.RunReport) {
		fmt.Fprintf(env.Stderr, "%s run %d: %s (%.1fs)\n", side, run.Run, run.Outcome, run.Seconds)
	}
	opts.Warn = func(message string) { fmt.Fprintf(env.Stderr, "elprovo: warning: %s\n", message) }
	report, err := replay.Run(ctx, plan, opts)
	if err != nil {
		fmt.Fprintf(env.Stderr, "elprovo: %v\n", err)
		return 2
	}
	out.results(report)
	if *require && report.StoppedEarly == "" && !report.HasFixed() {
		fmt.Fprintln(out.w, "No test is fixed, and --require is on, so this is a failure.")
	}
	return report.ExitStatus(*require)
}

// indent lines up the lines under a test with its name.
const indent = "                  "

// maxListed is the most tests that the output lists among the other tests.
const maxListed = 20

type output struct {
	w   io.Writer
	dir string
}

func (o output) header(plan replay.Plan, runs int, dryRun bool) {
	note := ""
	if dryRun {
		note = " (dry run)"
	}
	packages := "none"
	if len(plan.Packages) > 0 {
		packages = config.EscapeControl(strings.Join(plan.Packages, " "))
	}
	fmt.Fprintf(o.w, "base %s  head %s  %s each%s\npackages: %s\n\n", plan.Base[:7], plan.Head[:7], count(runs, "run"), note, packages)
}

func (o output) dryRun(plan replay.Plan) {
	if len(plan.Changed) == 0 {
		fmt.Fprintln(o.w, "The change adds or edits no test.")
	}
	for _, t := range plan.Changed {
		fmt.Fprintf(o.w, "changed  %s  %s\n", t.Name, config.EscapeControl(t.File))
	}
	fmt.Fprintln(o.w, "\nDry run: no command ran.")
}

var marks = map[replay.Verdict]string{
	replay.Fixed:         "✓",
	replay.Kept:          "✓",
	replay.NotFixed:      "✗",
	replay.NotReproduced: "✗",
	replay.Flaky:         "✗",
	replay.Broke:         "✗",
}

// listed are the verdicts of the other tests that the output lists by name.
var listed = []replay.Verdict{replay.Fixed, replay.Broke, replay.Flaky, replay.AlreadyFailing, replay.NotRun}

func (o output) results(report replay.Report) {
	for _, t := range report.Changed {
		o.test(report, t, t.File)
	}
	n := 0
	for _, t := range report.Regressions {
		if !slices.Contains(listed, t.Verdict) {
			continue
		}
		if n++; n <= maxListed {
			o.test(report, t, t.Package)
		}
	}
	if n > maxListed {
		fmt.Fprintf(o.w, "%d more of the other tests are not shown; report.json lists every test.\n\n", n-maxListed)
	}

	if report.StoppedEarly != "" {
		o.stoppedEarly(report)
	}
	fmt.Fprintf(o.w, "Changed tests: %s.\n", tally(report.Changed,
		replay.Fixed, replay.New, replay.NotReproduced, replay.NotFixed, replay.Flaky, replay.Skipped))
	fmt.Fprintf(o.w, "Other tests: %s.\n", tally(report.Regressions,
		replay.Kept, replay.Fixed, replay.Broke, replay.Flaky, replay.AlreadyFailing, replay.NotRun, replay.Skipped))
	fmt.Fprintf(o.w, "Report: %s\n", o.rel(filepath.Join(report.Dir, "report.json")))
}

// test prints one test's block: its verdict, how its runs ended, and the
// message of the run that explains the verdict.
func (o output) test(report replay.Report, t replay.TestReport, where string) {
	mark, ok := marks[t.Verdict]
	if !ok {
		mark = "!"
	}
	verdict := strings.ReplaceAll(string(t.Verdict), "-", " ")
	if t.Verdict == replay.Unfinished {
		verdict = "no verdict"
	}
	fmt.Fprintf(o.w, "%s %-15s %s  %s\n", mark, verdict, t.Name, config.EscapeControl(where))
	fmt.Fprintf(o.w, "%sbase: %s   head: %s\n", indent, outcomes(t.Base), outcomes(t.Head))
	if side, n, run, ok := explaining(t); ok {
		message := run.Message
		if message == "" {
			message = "no message"
		}
		fmt.Fprintf(o.w, "%s%s run %d: %s\n", indent, side, n, message)
		if slices.Contains([]replay.Verdict{replay.NotFixed, replay.Flaky, replay.Broke}, t.Verdict) {
			runs := report.BaseRuns
			if side == "head" {
				runs = report.HeadRuns
			}
			if log := runs[n-1].Log; log != "" {
				fmt.Fprintf(o.w, "%slog: %s\n", indent, o.rel(filepath.Join(report.Dir, log)))
			}
		}
	}
	fmt.Fprintln(o.w)
}

// explains says, for each verdict, which run's message explains it: the
// first run, in the order the runs ran, for which the function is true.
var explains = map[replay.Verdict]func(side string, o gotest.Outcome) bool{
	replay.Fixed:          func(side string, o gotest.Outcome) bool { return side == "base" && o == gotest.Failed },
	replay.New:            func(side string, o gotest.Outcome) bool { return side == "base" && o == gotest.Absent },
	replay.NotFixed:       func(side string, o gotest.Outcome) bool { return side == "head" && o != gotest.Passed },
	replay.Broke:          func(side string, o gotest.Outcome) bool { return side == "head" && o == gotest.Failed },
	replay.AlreadyFailing: func(side string, o gotest.Outcome) bool { return side == "head" && o == gotest.Failed },
	replay.Flaky:          func(_ string, o gotest.Outcome) bool { return o == gotest.Failed || o == gotest.Absent },
	replay.NotRun:         func(_ string, o gotest.Outcome) bool { return o == gotest.Absent },
	replay.Skipped:        func(_ string, o gotest.Outcome) bool { return o == gotest.Skipped },
}

// explaining finds the run whose message explains t's verdict: base run 1,
// head run 1, base run 2, and so on.
func explaining(t replay.TestReport) (string, int, replay.TestRun, bool) {
	match, ok := explains[t.Verdict]
	if !ok {
		return "", 0, replay.TestRun{}, false
	}
	for i := range max(len(t.Base), len(t.Head)) {
		if i < len(t.Base) && match("base", t.Base[i].Outcome) {
			return "base", i + 1, t.Base[i], true
		}
		if i < len(t.Head) && match("head", t.Head[i].Outcome) {
			return "head", i + 1, t.Head[i], true
		}
	}
	return "", 0, replay.TestRun{}, false
}

func (o output) stoppedEarly(report replay.Report) {
	fmt.Fprintf(o.w, "The replay stopped early: %s.\n", report.StoppedEarly)
	for _, side := range []struct {
		name string
		runs []replay.RunReport
	}{{"base", report.BaseRuns}, {"head", report.HeadRuns}} {
		for _, run := range side.runs {
			if run.Outcome != replay.RunNotRun {
				continue
			}
			if run.LastLine == "" {
				fmt.Fprintf(o.w, "%s run %d printed nothing.\n", side.name, run.Run)
			} else {
				fmt.Fprintf(o.w, "%s run %d ended with: %s\n", side.name, run.Run, run.LastLine)
			}
			if run.Log != "" {
				fmt.Fprintf(o.w, "log: %s\n", o.rel(filepath.Join(report.Dir, run.Log)))
			}
		}
	}
	fmt.Fprintln(o.w)
}

// outcomes says how a test ended in a side's runs, such as "failed 2 of 2".
func outcomes(runs []replay.TestRun) string {
	if len(runs) == 0 {
		return "no runs"
	}
	counts := map[gotest.Outcome]int{}
	for _, run := range runs {
		counts[run.Outcome]++
	}
	var parts []string
	for _, outcome := range []gotest.Outcome{gotest.Passed, gotest.Failed, gotest.Skipped, gotest.Absent} {
		if c := counts[outcome]; c == len(runs) {
			return fmt.Sprintf("%s %d of %d", outcome, c, c)
		} else if c > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", outcome, c))
		}
	}
	return fmt.Sprintf("%s of %d", strings.Join(parts, ", "), len(runs))
}

// tally counts the tests by verdict, in the order of verdicts.
func tally(tests []replay.TestReport, verdicts ...replay.Verdict) string {
	if len(tests) == 0 {
		return "none"
	}
	counts := map[replay.Verdict]int{}
	for _, t := range tests {
		counts[t.Verdict]++
	}
	var parts []string
	for _, v := range append(verdicts, replay.Unfinished) {
		if c := counts[v]; c > 0 {
			name := strings.ReplaceAll(string(v), "-", " ")
			if v == replay.Unfinished {
				name = "without a verdict"
			}
			parts = append(parts, fmt.Sprintf("%d %s", c, name))
		}
	}
	return strings.Join(parts, ", ")
}

func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (o output) rel(path string) string {
	if rel, err := filepath.Rel(o.dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}
