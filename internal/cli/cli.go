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
	"strings"

	"github.com/elprovo/elprovo/internal/replay"
)

// Env is what the command gets from the process that runs it.
type Env struct {
	Stdout, Stderr io.Writer
	// Environ is the process environment, as os.Environ gives it.
	Environ []string
	// Dir is the working directory.
	Dir string
}

const usage = `Elprovo replays the proofs that come with a change, on fresh copies of the
code without the change and with it, and reports which claims it verified.

Usage:
  elprovo replay [options] [PROOF...]   replay proofs; see elprovo replay -h
  elprovo version                       print the version

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
	flags.BoolVar(&opts.All, "all", false, "also replay the proofs that the change does not add or modify, on the head commit only")
	flags.IntVar(&opts.Runs, "runs", 2, "the number of runs on each commit, from 1 to 10")
	require := flags.Bool("require", false, "treat a replay with no selected proof as a failure")
	flags.StringVar(&opts.Out, "out", ".elprovo", "the directory for logs, artifacts and the report")
	dryRun := flags.Bool("dry-run", false, "check the files and print the selected proofs, without running any command")
	flags.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: elprovo replay [options] [PROOF...]\n\nOptions come before PROOF arguments.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	opts.Proofs = flags.Args()
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
	if len(plan.Proofs) == 0 {
		out.noProofs(opts.All, *require)
		if *require {
			return 1
		}
		return 0
	}
	if *dryRun {
		out.dryRun(plan)
		return 0
	}

	opts.RunDone = func(p replay.ProofReport, side string, run replay.RunReport) {
		fmt.Fprintf(env.Stderr, "%s  %s run %d: %s (%.1fs)\n", p.File, side, run.Run, run.Outcome, run.Seconds)
	}
	opts.ProofDone = out.proof
	opts.Warn = func(message string) { fmt.Fprintf(env.Stderr, "elprovo: warning: %s\n", message) }
	report, err := replay.Run(ctx, plan, opts)
	if err != nil {
		fmt.Fprintf(env.Stderr, "elprovo: %v\n", err)
		return 2
	}
	out.summary(report)
	return report.ExitStatus(*require)
}

type output struct {
	w   io.Writer
	dir string
}

func (o output) header(plan replay.Plan, runs int, dryRun bool) {
	note := ""
	if dryRun {
		note = " (dry run)"
	}
	fmt.Fprintf(o.w, "base %s  head %s  %s, %s each%s\n\n",
		plan.Base[:7], plan.Head[:7], count(len(plan.Proofs), "proof"), count(runs, "run"), note)
}

func (o output) noProofs(all, require bool) {
	if all {
		fmt.Fprintln(o.w, "No proof is selected: the head commit has no proof file.")
	} else {
		fmt.Fprintln(o.w, "No proof is selected: the change adds or modifies no proof file.")
	}
	if require {
		fmt.Fprintln(o.w, "--require is on, so this is a failure.")
	}
}

func (o output) dryRun(plan replay.Plan) {
	for _, p := range plan.Proofs {
		where := "base and head"
		if !p.OnBase {
			where = "head only: " + headOnlyReason(p.Claimed)
		}
		fmt.Fprintf(o.w, "%s  %s\n%s%s\n", p.File, where, indent, p.Claim)
	}
	fmt.Fprintln(o.w, "\nDry run: no command ran.")
}

const indent = "            "

var marks = map[replay.Result]string{
	replay.Verified:     "✓",
	replay.Held:         "✓",
	replay.Failed:       "✗",
	replay.Flaky:        "✗",
	replay.NoEffect:     "✗",
	replay.ResultNotRun: "!",
}

func (o output) proof(p replay.ProofReport) {
	fmt.Fprintf(o.w, "%s %-9s %s\n%s%s\n", marks[p.Result], p.Result, p.File, indent, p.Claim)
	sides := ""
	if len(p.Base) > 0 {
		sides = "base: " + tally(p.Base) + "   "
	}
	sides += "head: " + tally(p.Head)
	if p.Result == replay.Held {
		sides += "   not compared with the base: " + headOnlyReason(p.Claimed)
	}
	fmt.Fprintf(o.w, "%s%s\n", indent, sides)

	switch p.Result {
	case replay.Failed, replay.Flaky:
		// A flaky proof can fail on the base commit only.
		if run, ok := first(p.Head, replay.Fail); ok {
			fmt.Fprintf(o.w, "%slog: %s\n", indent, o.rel(filepath.Join(run.LogDir, "proof.log")))
		} else if run, ok := first(p.Base, replay.Fail); ok {
			fmt.Fprintf(o.w, "%slog: %s\n", indent, o.rel(filepath.Join(run.LogDir, "proof.log")))
		}
	case replay.ResultNotRun:
		if run, ok := first(inRunOrder(p), replay.NotRun); ok {
			// The reason starts with the command that failed: start or ready.
			log := strings.Fields(run.Reason + " start")[0]
			fmt.Fprintf(o.w, "%sreason: %s\n%slog: %s\n", indent, run.Reason, indent, o.rel(filepath.Join(run.LogDir, log+".log")))
		}
	}
	fmt.Fprintln(o.w)
}

func first(runs []replay.RunReport, outcome replay.Outcome) (replay.RunReport, bool) {
	for _, run := range runs {
		if run.Outcome == outcome {
			return run, true
		}
	}
	return replay.RunReport{}, false
}

// inRunOrder lists a proof's runs in the order they ran: base 1, head 1,
// base 2, head 2, and so on.
func inRunOrder(p replay.ProofReport) []replay.RunReport {
	var runs []replay.RunReport
	for i := 0; i < max(len(p.Base), len(p.Head)); i++ {
		if i < len(p.Base) {
			runs = append(runs, p.Base[i])
		}
		if i < len(p.Head) {
			runs = append(runs, p.Head[i])
		}
	}
	return runs
}

func (o output) summary(report replay.Report) {
	order := []replay.Result{replay.Verified, replay.Held, replay.Failed, replay.Flaky, replay.NoEffect, replay.ResultNotRun}
	counts := map[replay.Result]int{}
	unfinished := 0
	for _, p := range report.Proofs {
		if p.Result == replay.Unfinished {
			unfinished++
			continue
		}
		counts[p.Result]++
	}
	var parts []string
	for _, result := range order {
		if counts[result] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[result], result))
		}
	}
	if unfinished > 0 {
		parts = append(parts, fmt.Sprintf("%d without a result", unfinished))
	}
	fmt.Fprintf(o.w, "%s.\n", strings.Join(parts, ", "))
	if report.StoppedEarly != "" {
		fmt.Fprintf(o.w, "The replay stopped early: %s.\n", report.StoppedEarly)
	}
	fmt.Fprintf(o.w, "Report: %s\n", o.rel(filepath.Join(report.Dir, "report.json")))
}

func headOnlyReason(claimed bool) string {
	if claimed {
		return "before is skip"
	}
	return "the change does not add or modify this proof"
}

// tally says how a side's runs ended, such as "failed 2 of 2".
func tally(runs []replay.RunReport) string {
	counts := map[replay.Outcome]int{}
	for _, run := range runs {
		counts[run.Outcome]++
	}
	total := len(runs)
	switch {
	case counts[replay.Pass] == total:
		return fmt.Sprintf("passed %d of %d", total, total)
	case counts[replay.Fail] == total:
		return fmt.Sprintf("failed %d of %d", total, total)
	}
	var parts []string
	for _, item := range []struct {
		outcome replay.Outcome
		word    string
	}{{replay.Pass, "passed"}, {replay.Fail, "failed"}, {replay.NotRun, "not run"}} {
		if counts[item.outcome] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", item.word, counts[item.outcome]))
		}
	}
	return fmt.Sprintf("%s of %d", strings.Join(parts, ", "), total)
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
