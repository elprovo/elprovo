package replay

import "github.com/elprovo/elprovo/internal/config"

// Outcome is how one run of a proof ended.
type Outcome string

const (
	Pass   Outcome = "pass"
	Fail   Outcome = "fail"
	NotRun Outcome = "not-run"
)

// Result is what all the runs of a proof show, from section 3.3 of SPEC.md.
type Result string

const (
	Verified     Result = "verified"
	Held         Result = "held"
	Failed       Result = "failed"
	Flaky        Result = "flaky"
	NoEffect     Result = "no-effect"
	ResultNotRun Result = "not-run"
	// Unfinished is the result of a proof whose runs did not all end,
	// because the replay stopped early.
	Unfinished Result = ""
)

// RunReport is one run of a proof on one commit.
type RunReport struct {
	Run              int     `json:"run"`
	Outcome          Outcome `json:"outcome"`
	ExitCode         *int    `json:"exit_code"`
	TimedOut         bool    `json:"timed_out"`
	Seconds          float64 `json:"seconds"`
	Reason           string  `json:"reason"`
	ArtifactsDropped bool    `json:"artifacts_dropped"`
	Dir              string  `json:"dir"`
	// LogDir is the absolute path of the run's directory. It is not in the
	// report, which uses paths relative to the replay's directory.
	LogDir string `json:"-"`
}

// ProofReport is every run of one proof and its result.
type ProofReport struct {
	File    string        `json:"file"`
	Claim   string        `json:"claim"`
	Before  config.Before `json:"before"`
	Claimed bool          `json:"claimed"`
	Result  Result        `json:"result"`
	Base    []RunReport   `json:"base"`
	Head    []RunReport   `json:"head"`
}

// Report is the result of one replay, as report.json holds it.
type Report struct {
	Format       int           `json:"format"`
	Base         string        `json:"base"`
	Head         string        `json:"head"`
	Runs         int           `json:"runs"`
	StoppedEarly string        `json:"stopped_early"`
	Proofs       []ProofReport `json:"proofs"`
	// Dir is the replay's output directory. It is empty after a dry run.
	Dir string `json:"-"`
}

// resultOf applies the rules of section 3.3 of SPEC.md, in order. base is
// empty when the proof did not run on the base commit.
func resultOf(head, base []Outcome) Result {
	switch {
	case anyIs(head, NotRun):
		return ResultNotRun
	case anyIs(head, Pass) && anyIs(head, Fail):
		return Flaky
	case !anyIs(head, Pass):
		return Failed
	case len(base) == 0:
		return Held
	case anyIs(base, NotRun):
		return ResultNotRun
	case anyIs(base, Pass) && anyIs(base, Fail):
		return Flaky
	case anyIs(base, Pass):
		return NoEffect
	default:
		return Verified
	}
}

func anyIs(outcomes []Outcome, want Outcome) bool {
	for _, o := range outcomes {
		if o == want {
			return true
		}
	}
	return false
}

// ExitStatus is the exit status of section 3.4 of SPEC.md for a replay that
// ran. A replay that could not start exits 2 without a report.
func (r Report) ExitStatus(require bool) int {
	if r.StoppedEarly != "" {
		return 2
	}
	if len(r.Proofs) == 0 {
		if require {
			return 1
		}
		return 0
	}
	notRun := false
	for _, p := range r.Proofs {
		switch p.Result {
		case Failed, Flaky, NoEffect:
			return 1
		case ResultNotRun:
			notRun = true
		}
	}
	if notRun {
		return 2
	}
	return 0
}
