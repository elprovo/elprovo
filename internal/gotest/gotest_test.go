package gotest

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// The testdata files hold real `go test -json -count=1` output from Go 1.27,
// with local paths replaced.

func parseFile(t *testing.T, name string) Results {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	results, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func wantResult(t *testing.T, results Results, test Test, outcome Outcome, message string) {
	t.Helper()
	got := results.Result(test)
	if got.Outcome != outcome || got.Message != message {
		t.Errorf("%s: %s %q; want %s %q", test.Name, got.Outcome, got.Message, outcome, message)
	}
}

func TestParseGivesEachTopLevelTestItsOutcomeAndMessage(t *testing.T) {
	results := parseFile(t, "results.jsonl")

	wantResult(t, results, Test{"gj/more", "TestPass"}, Passed, "")
	wantResult(t, results, Test{"gj/more", "TestFail"}, Failed, "x_test.go:6: total 81.00, want 90.00")
	wantResult(t, results, Test{"gj/more", "TestSkip"}, Skipped, "x_test.go:7: needs a network")
	wantResult(t, results, Test{"gj/more", "TestSub"}, Failed, "x_test.go:10: sub b failed")
	if _, ok := results.Tests[Test{"gj/more", "TestSub/a"}]; ok {
		t.Error("a subtest has its own result; want only top-level tests")
	}
}

func TestParseMarksTheTestsAfterAPanicAbsent(t *testing.T) {
	results := parseFile(t, "panic.jsonl")

	wantResult(t, results, Test{"gj/ok", "TestFail"}, Failed, "x_test.go:6: total 81.00, want 90.00")
	wantResult(t, results, Test{"gj/ok", "TestPanic"}, Failed, "panic: assignment to entry in nil map [recovered, repanicked]")
	wantResult(t, results, Test{"gj/ok", "TestSkip"}, Absent, "panic: assignment to entry in nil map [recovered, repanicked]")
}

func TestParseGivesTheCompileErrorOfAPackageThatDidNotBuild(t *testing.T) {
	results := parseFile(t, "build-failure.jsonl")

	wantResult(t, results, Test{"gj/broken", "TestUsesNewAPI"}, Absent, "broken/x_test.go:5:41: undefined: NewThing")
}

func TestParseGivesTheExitOfAPackageWhoseTestMainExited(t *testing.T) {
	results := parseFile(t, "testmain-exit.jsonl")

	wantResult(t, results, Test{"gj/mainfail", "TestNeverRuns"}, Absent, "exit status 3")
}

func TestParseFailsATestThatPanicsWithoutAnEnd(t *testing.T) {
	// go test -timeout and a panic in a goroutine end the test binary
	// without an end event for the test that was running.
	hang := parseFile(t, "hang.jsonl")
	wantResult(t, hang, Test{"gj/hang", "TestBefore"}, Passed, "")
	wantResult(t, hang, Test{"gj/hang", "TestHang"}, Failed, "panic: test timed out after 2s")
	wantResult(t, hang, Test{"gj/hang", "TestAfter"}, Absent, "panic: test timed out after 2s")

	background := parseFile(t, "gopanic.jsonl")
	wantResult(t, background, Test{"gj/gopanic", "TestBackground"}, Failed, "panic: boom in a goroutine")
	wantResult(t, background, Test{"gj/gopanic", "TestAfter"}, Absent, "panic: boom in a goroutine")
}

func TestParseKeepsAFailureOfATestThatRanSeveralTimes(t *testing.T) {
	// The output of go test -count=2: the test failed, and then passed.
	results := parseFile(t, "count.jsonl")

	wantResult(t, results, Test{"gj/count", "TestFirstFails"}, Failed, "x_test.go:10: first run fails")
}

func TestParseMergesTwoResultsOfOneName(t *testing.T) {
	// Package p and package p_test in one directory can both declare TestX,
	// and go test prints both under one name.
	input := `{"Action":"pass","Package":"p","Test":"TestPassThenSkip"}
{"Action":"skip","Package":"p","Test":"TestPassThenSkip"}
{"Action":"output","Package":"p","Test":"TestFailThenPass","Output":"    a_test.go:3: wrong\n"}
{"Action":"fail","Package":"p","Test":"TestFailThenPass"}
{"Action":"pass","Package":"p","Test":"TestFailThenPass"}
`
	results, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}

	wantResult(t, results, Test{"p", "TestPassThenSkip"}, Passed, "")
	wantResult(t, results, Test{"p", "TestFailThenPass"}, Failed, "a_test.go:3: wrong")
}

func TestParseSkipsTheRuleLinesOfTheRaceDetector(t *testing.T) {
	input := `{"Action":"output","Package":"p","Test":"TestRace","Output":"==================\n"}
{"Action":"output","Package":"p","Test":"TestRace","Output":"WARNING: DATA RACE\n"}
{"Action":"fail","Package":"p","Test":"TestRace"}
`
	results, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}

	wantResult(t, results, Test{"p", "TestRace"}, Failed, "WARNING: DATA RACE")
}

func TestParseAddsTheNextLinesToALineWithOnlyALocation(t *testing.T) {
	// testify prints the location alone on the first line.
	results := parseFile(t, "tfy.jsonl")

	got := results.Result(Test{"gj/tfy", "TestEqual"})
	want := `x_test.go:10: Error: Not equal: expected: "90.00" actual : "81.00"`
	if got.Outcome != Failed || !strings.HasPrefix(got.Message, want) {
		t.Errorf("result is %s %q; want failed, starting with %q", got.Outcome, got.Message, want)
	}
}

func TestParseCutsALongMessageAndSkipsLinesThatAreNotEvents(t *testing.T) {
	long := strings.Repeat("x", 1500)
	input := "go: downloading example.com/m v1.0.0\n" +
		`{"note":"a JSON line that is not an event"}` + "\n" +
		`{"Action":"run","Package":"p","Test":"TestLong"}` + "\n" +
		`{"Action":"output","Package":"p","Test":"TestLong","Output":"    a_test.go:3: ` + long + `\n"}` + "\n" +
		`{"Action":"fail","Package":"p","Test":"TestLong"}` + "\n"

	results, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}

	got := results.Result(Test{"p", "TestLong"})
	if got.Outcome != Failed || len([]rune(got.Message)) != maxMessage || !strings.HasPrefix(got.Message, "a_test.go:3: xxx") {
		t.Errorf("result is %s with a message of %d characters; want failed with %d characters", got.Outcome, len([]rune(got.Message)), maxMessage)
	}
	if results.Events != 3 {
		t.Errorf("Events is %d; want 3, one for each line that is an event", results.Events)
	}
}

func TestFuncsListsTheTopLevelTestFunctions(t *testing.T) {
	src := `package p

import "testing"

func TestA(t *testing.T) {}
func Test(t *testing.T) {}
func Testable(t *testing.T) {}
func TestMain(m *testing.M) {}
func TestHelper(t *testing.T, n int) {}
func helper(t *testing.T) {}

type s struct{}

func (s) TestMethod(t *testing.T) {}
`
	funcs, err := Funcs([]byte(src))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, f := range funcs {
		names = append(names, f.Name)
	}
	if want := []string{"TestA", "Test"}; !slices.Equal(names, want) {
		t.Errorf("tests are %v; want %v", names, want)
	}
}

func TestChangedFindsNewAndEditedTestsButNotCommentsOrSpacing(t *testing.T) {
	base := `package p

import "testing"

func TestKept(t *testing.T) { if 1 != 1 { t.Fatal("no") } }

func TestEdited(t *testing.T) { t.Log("old") }
`
	head := `package p

import "testing"

// TestKept now has a comment, and its body has other spacing.
func TestKept(t *testing.T) {
	// A comment on its own line.

	if 1 != 1 {
		t.Fatal("no") // A comment at the end of a line.
	}
}

func TestEdited(t *testing.T) { t.Log("new") }

func TestAdded(t *testing.T) {}
`
	changed, err := Changed([][]byte{[]byte(base)}, []byte(head))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"TestAdded", "TestEdited"}; !slices.Equal(changed, want) {
		t.Errorf("changed tests are %v; want %v", changed, want)
	}
}

func TestChangedComparesWithEveryTestFileOfThePackage(t *testing.T) {
	// TestMoved moves, unchanged, from a_test.go to the new file b_test.go.
	baseA := "package p\n\nimport \"testing\"\n\nfunc TestMoved(t *testing.T) { t.Log(1) }\n"
	baseOther := "package p\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) {}\n"
	head := "package p\n\nimport \"testing\"\n\nfunc TestMoved(t *testing.T) { t.Log(1) }\n\nfunc TestNew(t *testing.T) {}\n"

	changed, err := Changed([][]byte{[]byte(baseA), []byte(baseOther)}, []byte(head))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"TestNew"}; !slices.Equal(changed, want) {
		t.Errorf("changed tests are %v; want %v", changed, want)
	}
}

func TestChangedSkipsABaseFileThatDoesNotParse(t *testing.T) {
	head := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"

	changed, err := Changed([][]byte{[]byte("package p\nfunc TestA(t *testing.T) {")}, []byte(head))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"TestA"}; !slices.Equal(changed, want) {
		t.Errorf("changed tests are %v; want %v", changed, want)
	}
}

func TestChangedTreatsEveryTestAsNewWhenTheFileIsNew(t *testing.T) {
	head := "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\nfunc TestA(t *testing.T) {}\n"

	changed, err := Changed(nil, []byte(head))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"TestA", "TestB"}; !slices.Equal(changed, want) {
		t.Errorf("changed tests are %v; want %v", changed, want)
	}
}

func TestChangedReportsAFileThatDoesNotParse(t *testing.T) {
	if _, err := Changed(nil, []byte("package p\nfunc TestA(t *testing.T) {")); err == nil {
		t.Error("no error for a file that does not parse")
	}
}
