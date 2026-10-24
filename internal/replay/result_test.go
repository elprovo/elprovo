package replay

import (
	"testing"

	"github.com/elprovo/elprovo-next/internal/gotest"
)

const (
	pass   = gotest.Passed
	fail   = gotest.Failed
	skip   = gotest.Skipped
	absent = gotest.Absent
)

func TestChangedTestVerdictsApplyInOrder(t *testing.T) {
	tests := []struct {
		name       string
		head, base []gotest.Outcome
		want       Verdict
	}{
		{"fails without the change and passes with it", []gotest.Outcome{pass, pass}, []gotest.Outcome{fail, fail}, Fixed},
		{"passes without the change too", []gotest.Outcome{pass, pass}, []gotest.Outcome{pass, pass}, NotReproduced},
		{"has no result without the change", []gotest.Outcome{pass, pass}, []gotest.Outcome{absent, absent}, New},
		{"fails with the change", []gotest.Outcome{fail, fail}, []gotest.Outcome{fail, fail}, NotFixed},
		{"has no result with the change", []gotest.Outcome{absent, absent}, []gotest.Outcome{fail, fail}, NotFixed},
		{"passes and fails with the change", []gotest.Outcome{pass, fail}, []gotest.Outcome{fail, fail}, Flaky},
		{"passes once and has no result once with the change", []gotest.Outcome{absent, pass}, []gotest.Outcome{fail, fail}, Flaky},
		{"fails and passes without the change", []gotest.Outcome{pass, pass}, []gotest.Outcome{fail, pass}, Flaky},
		{"fails and has no result without the change", []gotest.Outcome{pass, pass}, []gotest.Outcome{absent, fail}, Flaky},
		{"is skipped with the change", []gotest.Outcome{skip, skip}, []gotest.Outcome{fail, fail}, Skipped},
		{"is skipped without the change", []gotest.Outcome{pass, pass}, []gotest.Outcome{skip, skip}, Skipped},
		{"is skipped once with the change", []gotest.Outcome{pass, skip}, []gotest.Outcome{fail, fail}, Flaky},
		{"a failure with the change comes before a skip", []gotest.Outcome{fail, fail}, []gotest.Outcome{skip, skip}, NotFixed},
	}
	for _, tt := range tests {
		if got := changedVerdict(tt.head, tt.base); got != tt.want {
			t.Errorf("%s: changedVerdict(head %v, base %v) = %q; want %q", tt.name, tt.head, tt.base, got, tt.want)
		}
	}
}

func TestRegressionVerdictsApplyInOrder(t *testing.T) {
	tests := []struct {
		name       string
		head, base []gotest.Outcome
		want       Verdict
	}{
		{"passes on both", []gotest.Outcome{pass, pass}, []gotest.Outcome{pass, pass}, Kept},
		{"passes without the change and fails with it", []gotest.Outcome{fail, fail}, []gotest.Outcome{pass, pass}, Broke},
		{"fails on both", []gotest.Outcome{fail, fail}, []gotest.Outcome{fail, fail}, AlreadyFailing},
		{"fails without the change and passes with it", []gotest.Outcome{pass, pass}, []gotest.Outcome{fail, fail}, Fixed},
		{"has no result with the change", []gotest.Outcome{absent, absent}, []gotest.Outcome{pass, pass}, NotRun},
		{"has no result once without the change", []gotest.Outcome{pass, pass}, []gotest.Outcome{pass, absent}, NotRun},
		{"no result comes before disagreement", []gotest.Outcome{pass, fail}, []gotest.Outcome{absent, pass}, NotRun},
		{"passes and fails with the change", []gotest.Outcome{pass, fail}, []gotest.Outcome{pass, pass}, Flaky},
		{"passes and fails without the change", []gotest.Outcome{pass, pass}, []gotest.Outcome{fail, pass}, Flaky},
		{"is skipped on both", []gotest.Outcome{skip, skip}, []gotest.Outcome{skip, skip}, Skipped},
		{"is skipped with the change", []gotest.Outcome{skip, skip}, []gotest.Outcome{pass, pass}, Skipped},
	}
	for _, tt := range tests {
		if got := regressionVerdict(tt.head, tt.base); got != tt.want {
			t.Errorf("%s: regressionVerdict(head %v, base %v) = %q; want %q", tt.name, tt.head, tt.base, got, tt.want)
		}
	}
}

func TestExitStatus(t *testing.T) {
	report := func(stopped string, changed []Verdict, regressions ...Verdict) Report {
		r := Report{StoppedEarly: stopped}
		for _, v := range changed {
			r.Changed = append(r.Changed, TestReport{Verdict: v})
		}
		for _, v := range regressions {
			r.Regressions = append(r.Regressions, TestReport{Verdict: v})
		}
		return r
	}
	tests := []struct {
		name    string
		report  Report
		require bool
		want    int
	}{
		{"fixed and kept", report("", []Verdict{Fixed}, Kept), false, 0},
		{"new, skipped, already failing and not run", report("", []Verdict{New, Skipped}, AlreadyFailing, NotRun), false, 0},
		{"no test", report("", nil), false, 0},
		{"no fixed test with --require", report("", []Verdict{New}, Kept), true, 1},
		{"a fixed regression test with --require", report("", nil, Fixed), true, 0},
		{"not fixed", report("", []Verdict{Fixed, NotFixed}), false, 1},
		{"not reproduced", report("", []Verdict{NotReproduced}), false, 1},
		{"a flaky changed test", report("", []Verdict{Flaky}), false, 1},
		{"broke", report("", []Verdict{Fixed}, Kept, Broke), false, 1},
		{"a flaky regression test", report("", nil, Flaky), false, 1},
		{"stopped early beats not fixed", report("interrupted", []Verdict{NotFixed}), false, 2},
	}
	for _, tt := range tests {
		if got := tt.report.ExitStatus(tt.require); got != tt.want {
			t.Errorf("%s: exit status %d; want %d", tt.name, got, tt.want)
		}
	}
}
