package replay

import "testing"

func TestResultRulesApplyInOrder(t *testing.T) {
	tests := []struct {
		name       string
		head, base []Outcome
		want       Result
	}{
		{"rule 1: a head run did not run", []Outcome{Pass, NotRun}, []Outcome{Fail, Fail}, ResultNotRun},
		{"rule 2: head runs disagree", []Outcome{Pass, Fail}, []Outcome{Fail, Fail}, Flaky},
		{"rule 3: every head run fails", []Outcome{Fail, Fail}, []Outcome{Pass, Pass}, Failed},
		{"rule 4: no base runs", []Outcome{Pass, Pass}, nil, Held},
		{"rule 5: a base run did not run", []Outcome{Pass, Pass}, []Outcome{Fail, NotRun}, ResultNotRun},
		{"rule 6: base runs disagree", []Outcome{Pass, Pass}, []Outcome{Pass, Fail}, Flaky},
		{"rule 7: base runs pass too", []Outcome{Pass, Pass}, []Outcome{Pass, Pass}, NoEffect},
		{"rule 8: fails on base, passes on head", []Outcome{Pass, Pass}, []Outcome{Fail, Fail}, Verified},
	}
	for _, tt := range tests {
		if got := resultOf(tt.head, tt.base); got != tt.want {
			t.Errorf("%s: resultOf(head %v, base %v) = %q; want %q", tt.name, tt.head, tt.base, got, tt.want)
		}
	}
}

func TestExitStatus(t *testing.T) {
	report := func(stopped string, results ...Result) Report {
		r := Report{StoppedEarly: stopped}
		for _, result := range results {
			r.Proofs = append(r.Proofs, ProofReport{Result: result})
		}
		return r
	}
	tests := []struct {
		name    string
		report  Report
		require bool
		want    int
	}{
		{"verified and held", report("", Verified, Held), false, 0},
		{"no proof", report(""), false, 0},
		{"no proof with --require", report(""), true, 1},
		{"a failed proof", report("", Verified, Failed), false, 1},
		{"a no-effect proof", report("", NoEffect), false, 1},
		{"a flaky proof beats a not-run proof", report("", ResultNotRun, Flaky), false, 1},
		{"a not-run proof", report("", Verified, ResultNotRun), false, 2},
		{"stopped early beats a failed proof", report("interrupted", Failed, Unfinished), false, 2},
	}
	for _, tt := range tests {
		if got := tt.report.ExitStatus(tt.require); got != tt.want {
			t.Errorf("%s: exit status %d; want %d", tt.name, got, tt.want)
		}
	}
}
