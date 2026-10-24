// Package gotest reads what `go test -json` prints, and finds the test
// functions in a Go test file.
package gotest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Outcome is how one top-level test ended in one run.
type Outcome string

const (
	Passed  Outcome = "passed"
	Failed  Outcome = "failed"
	Skipped Outcome = "skipped"
	// Absent means the run printed no result for the test: its package did
	// not build, its TestMain exited, or an earlier test stopped the package.
	Absent Outcome = "absent"
)

// Test names a top-level test function. Package is the import path, as go
// test prints it.
type Test struct {
	Package string
	Name    string
}

// Result is a test's outcome in one run, with the first line of output that
// explains it. When that line holds only a location, such as testify prints,
// the lines after it follow.
type Result struct {
	Outcome Outcome
	Message string
}

// Results is what one run of go test -json printed.
type Results struct {
	Tests map[Test]Result
	// Reasons holds, for each package that failed, the first line that
	// explains why, such as a compile error or a panic.
	Reasons map[string]string
	// Events is the number of lines that were go test -json events. It is 0
	// when go test could not run at all.
	Events int
}

const (
	// maxMessage bounds the memory that one message takes. A report keeps
	// fewer characters.
	maxMessage = 1000
	maxLine    = 1 << 20
)

// rank orders the outcomes of one name: the higher one counts.
var rank = map[Outcome]int{Skipped: 0, Passed: 1, Failed: 2}

// locationOnly matches a line that names a place in a file and nothing else,
// such as "x_test.go:10:".
var locationOnly = regexp.MustCompile(`^[^\s:]+\.go:[0-9]+:$`)

// Result returns the result of t. A test without a result is Absent, with
// its package's reason as the message.
func (r Results) Result(t Test) Result {
	if result, ok := r.Tests[t]; ok {
		return result
	}
	return Result{Outcome: Absent, Message: r.Reasons[t.Package]}
}

type event struct {
	Action      string
	Package     string
	ImportPath  string
	Test        string
	Output      string
	FailedBuild string
}

// Parse reads go test -json output. It skips lines that are not events, such
// as messages from the go command, and the results of subtests, which count
// through their parent. A test that printed a panic and has no result, as
// after go test -timeout or a panic in a goroutine, failed. When one name has
// several results, as with -count or a test of the same name in package x
// and x_test, a failure counts over a pass, and a pass over a skip.
func Parse(r io.Reader) (Results, error) {
	results := Results{Tests: map[Test]Result{}, Reasons: map[string]string{}}
	firstLines := map[Test]string{}
	// continued holds the tests whose first line holds only a location.
	continued := map[Test]bool{}
	panicked := map[Test]bool{}
	buildErrors := map[string]string{}
	panics := map[string]string{}
	packageLines := map[string]string{}
	var ends []event

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || e.Action == "" {
			continue
		}
		results.Events++
		switch e.Action {
		case "build-output":
			if line := explaining(e.Output); line != "" {
				setFirst(buildErrors, packageOf(e.ImportPath), line)
			}
		case "output":
			line := explaining(e.Output)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "panic: ") {
				setFirst(panics, e.Package, line)
			}
			if e.Test == "" {
				setFirst(packageLines, e.Package, line)
				continue
			}
			t := Test{e.Package, parent(e.Test)}
			if strings.HasPrefix(line, "panic: ") {
				panicked[t] = true
			}
			switch first, ok := firstLines[t]; {
			case !ok:
				firstLines[t] = line
				continued[t] = locationOnly.MatchString(line)
			case continued[t] && !strings.HasPrefix(line, "Error Trace:"):
				firstLines[t] = cut(first + " " + strings.Join(strings.Fields(line), " "))
				continued[t] = utf8.RuneCountInString(firstLines[t]) < maxMessage
			}
		case "pass", "fail", "skip":
			ends = append(ends, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return Results{}, fmt.Errorf("read the output of go test: %w", err)
	}

	for _, e := range ends {
		if e.Test == "" {
			if e.Action == "fail" {
				results.Reasons[e.Package] = firstOf(buildErrors[packageOf(e.FailedBuild)], panics[e.Package], packageLines[e.Package], "the package failed before its tests ran")
			}
			continue
		}
		if strings.Contains(e.Test, "/") {
			continue
		}
		t := Test{e.Package, e.Test}
		result := Result{Outcome: map[string]Outcome{"pass": Passed, "fail": Failed, "skip": Skipped}[e.Action]}
		if result.Outcome != Passed {
			result.Message = firstLines[t]
		}
		if previous, ok := results.Tests[t]; ok && rank[previous.Outcome] >= rank[result.Outcome] {
			continue
		}
		results.Tests[t] = result
	}
	for t := range panicked {
		if _, ok := results.Tests[t]; !ok {
			results.Tests[t] = Result{Outcome: Failed, Message: firstLines[t]}
		}
	}
	return results, nil
}

// explaining returns line without its indent, or "" when the line is one of
// the markers that go test prints around every test, or a rule of = signs,
// as the race detector prints around its report.
func explaining(output string) string {
	line := strings.TrimSpace(output)
	if strings.Trim(line, "=") == "" {
		return ""
	}
	for _, marker := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- PASS", "--- FAIL", "--- SKIP", "PASS", "FAIL", "ok  ", "# "} {
		if strings.HasPrefix(line, marker) {
			return ""
		}
	}
	return cut(line)
}

func cut(text string) string {
	if runes := []rune(text); len(runes) > maxMessage {
		return string(runes[:maxMessage])
	}
	return text
}

func setFirst[K comparable](m map[K]string, key K, line string) {
	if _, ok := m[key]; !ok {
		m[key] = line
	}
}

// packageOf turns the import path of a test binary, such as
// "example.com/p [example.com/p.test]", into the package's import path.
func packageOf(importPath string) string {
	pkg, _, _ := strings.Cut(importPath, " ")
	return pkg
}

func parent(test string) string {
	name, _, _ := strings.Cut(test, "/")
	return name
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Func is a top-level test function in a Go file.
type Func struct {
	Name string
	// Decl is the function's declaration, printed without comments and
	// blank lines, so that two versions compare equal when only comments or
	// spacing differ.
	Decl string
}

// Funcs returns the top-level test functions in src, in source order: each
// func TestXxx(t *testing.T), as go test finds them.
func Funcs(src []byte) ([]Func, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return nil, err
	}
	var funcs []Func
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isTestName(fn.Name.Name) || !takesT(fn.Type) {
			continue
		}
		var decl bytes.Buffer
		if err := printer.Fprint(&decl, fset, fn); err != nil {
			return nil, err
		}
		// The printer keeps a blank line where a comment was.
		lines := strings.Split(decl.String(), "\n")
		lines = slices.DeleteFunc(lines, func(line string) bool { return strings.TrimSpace(line) == "" })
		funcs = append(funcs, Func{Name: fn.Name.Name, Decl: strings.Join(lines, "\n")})
	}
	return funcs, nil
}

// isTestName applies the go test rule: Test, or Test followed by a character
// that is not a lowercase letter.
func isTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// takesT reports whether a function's only parameter is a *testing.T.
func takesT(fn *ast.FuncType) bool {
	if fn.Params == nil || len(fn.Params.List) != 1 || len(fn.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := fn.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

// Changed returns, in name order, the tests in head, one test file, that
// base lacks or declares differently. base holds the test files of the same
// package in the base commit, so a test that moves to another file does not
// change. A base file that does not parse adds no tests.
func Changed(base [][]byte, head []byte) ([]string, error) {
	headFuncs, err := Funcs(head)
	if err != nil {
		return nil, err
	}
	before := map[string]string{}
	for _, file := range base {
		baseFuncs, err := Funcs(file)
		if err != nil {
			continue
		}
		for _, f := range baseFuncs {
			before[f.Name] = f.Decl
		}
	}
	var changed []string
	for _, f := range headFuncs {
		if decl, ok := before[f.Name]; !ok || decl != f.Decl {
			changed = append(changed, f.Name)
		}
	}
	slices.Sort(changed)
	return changed, nil
}
