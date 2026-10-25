// Package config reads and checks elprovo.toml, as section 1 of SPEC.md
// defines it.
package config

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

// MaxFileSize is the largest elprovo.toml that Elprovo reads.
const MaxFileSize = 64 << 10

// RepoFile is the path of the repository file, relative to the repository root.
const RepoFile = "elprovo.toml"

// Packages is the placeholder in a suite's run for the packages to test.
const Packages = "{packages}"

// GoTestJSON is the only report format this release reads: the output of
// go test -json.
const GoTestJSON = "go-test-json"

// ErrVersion1 is the error for an elprovo.toml in the format of Elprovo 0.2.
var ErrVersion1 = errors.New("version 1 is the proof-file format of Elprovo 0.2; write version 3, as SPEC.md section 1 shows")

// Repo is a checked elprovo.toml.
type Repo struct {
	Env     map[string]string
	PassEnv []string
	Tests   Tests
	// Suites run in this order in every run.
	Suites []Suite
}

// Tests says which files hold the tests.
type Tests struct {
	// Files are the patterns of section 1.2 of SPEC.md.
	Files []string
}

// Suite is a command that runs tests, with the app that they need.
type Suite struct {
	Name    string
	Run     string
	Timeout time.Duration
	// App is nil when the suite needs nothing running.
	App *App
}

// TakesPackages reports whether the suite's command takes the packages that
// the change touches, in place of {packages}. A suite that does not runs on
// every replay.
func (s Suite) TakesPackages() bool {
	return strings.Contains(s.Run, Packages)
}

// App says how to start, check and stop an app that the tests need.
type App struct {
	Start        string
	Ready        string
	Stop         string
	StartTimeout time.Duration
	ReadyTimeout time.Duration
	StopTimeout  time.Duration
}

var (
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	suiteNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	durationPattern  = regexp.MustCompile(`^([1-9][0-9]*)([smh])$`)
)

type rawApp struct {
	Start    string `toml:"start"`
	Ready    string `toml:"ready"`
	Stop     string `toml:"stop"`
	Timeouts struct {
		Start string `toml:"start"`
		Ready string `toml:"ready"`
		Stop  string `toml:"stop"`
	} `toml:"timeouts"`
}

type rawSuite struct {
	Run     string `toml:"run"`
	Report  string `toml:"report"`
	Timeout string `toml:"timeout"`
	App     rawApp `toml:"app"`
}

// rawRepo holds both versions. Version 2 has run, report and timeout in
// [tests], and [app]; version 3 has them in [suites.<name>].
type rawRepo struct {
	Version *int              `toml:"version"`
	Env     map[string]string `toml:"env"`
	PassEnv []string          `toml:"pass_env"`
	Tests   struct {
		Files   []string `toml:"files"`
		Run     string   `toml:"run"`
		Report  string   `toml:"report"`
		Timeout string   `toml:"timeout"`
	} `toml:"tests"`
	App    rawApp              `toml:"app"`
	Suites map[string]rawSuite `toml:"suites"`
}

// ParseRepo checks the content of an elprovo.toml. name is the file's name in
// error messages.
func ParseRepo(name string, data []byte) (Repo, error) {
	var raw rawRepo
	meta, err := decode(data, &raw)
	if err != nil {
		return Repo{}, fmt.Errorf("%s: %w", name, err)
	}
	var problems []string
	for envName := range raw.Env {
		problems = checkEnvName(problems, "env", envName)
	}
	for _, envName := range raw.PassEnv {
		problems = checkEnvName(problems, "pass_env", envName)
	}
	repo := Repo{Env: raw.Env, PassEnv: raw.PassEnv, Tests: Tests{Files: raw.Tests.Files}}
	if len(repo.Tests.Files) == 0 {
		problems = append(problems, "tests.files is missing")
	}
	for _, pattern := range repo.Tests.Files {
		if !validPattern(pattern) {
			problems = append(problems, fmt.Sprintf("tests.files pattern %q is not valid", pattern))
		}
	}

	switch {
	case raw.Version == nil:
		problems = append(problems, "version is missing")
	case *raw.Version == 2:
		if meta.IsDefined("suites") {
			problems = append(problems, "suites is a table of version 3; write version 3, or put the command in [tests]")
		}
		// Version 2 has one suite: [tests] with [app]. Its command takes
		// the packages.
		tests := rawSuite{Run: raw.Tests.Run, Report: raw.Tests.Report, Timeout: raw.Tests.Timeout, App: raw.App}
		var suite Suite
		suite, problems = parseSuite(problems, "tests", "tests", "app", tests, meta.IsDefined("app"))
		if suite.Run != "" && !suite.TakesPackages() {
			problems = append(problems, "tests.run must contain "+Packages+", where Elprovo puts the packages to test")
		}
		repo.Suites = []Suite{suite}
	case *raw.Version == 3:
		for _, field := range []string{"run", "report", "timeout"} {
			if meta.IsDefined("tests", field) {
				problems = append(problems, fmt.Sprintf("tests.%s is a field of version 2; in version 3 it belongs to a suite, such as [suites.unit]", field))
			}
		}
		if meta.IsDefined("app") {
			problems = append(problems, "app is a table of version 2; in version 3 each suite has its own, such as [suites.e2e.app]")
		}
		for _, key := range meta.Keys() {
			if len(key) != 2 || key[0] != "suites" {
				continue
			}
			suiteName := key[1]
			if !suiteNamePattern.MatchString(suiteName) {
				problems = append(problems, fmt.Sprintf("suite name %q must match [a-z0-9][a-z0-9-]*", EscapeControl(suiteName)))
				continue
			}
			prefix := "suites." + suiteName
			var suite Suite
			suite, problems = parseSuite(problems, suiteName, prefix, prefix+".app", raw.Suites[suiteName], meta.IsDefined("suites", suiteName, "app"))
			repo.Suites = append(repo.Suites, suite)
		}
		if len(raw.Suites) == 0 {
			problems = append(problems, "suites is missing; add at least one, such as [suites.unit]")
		}
	default:
		problems = append(problems, fmt.Sprintf("version %d is not supported; this release reads versions 2 and 3", *raw.Version))
	}
	if len(problems) > 0 {
		return Repo{}, fmt.Errorf("%s: %s", name, strings.Join(problems, "; "))
	}
	return repo, nil
}

// parseSuite checks one suite. prefix and appPrefix name its fields and its
// app's fields in messages.
func parseSuite(problems []string, name, prefix, appPrefix string, raw rawSuite, hasApp bool) (Suite, []string) {
	suite := Suite{Name: name, Run: raw.Run}
	problems = requireCommand(problems, prefix+".run", raw.Run)
	switch raw.Report {
	case "":
		problems = append(problems, prefix+".report is missing")
	case GoTestJSON:
	default:
		problems = append(problems, fmt.Sprintf("%s.report is %q; this release reads %s", prefix, raw.Report, GoTestJSON))
	}
	problems = parseDuration(problems, prefix+".timeout", raw.Timeout, 10*time.Minute, &suite.Timeout)
	if hasApp {
		app := raw.App
		suite.App = &App{Start: app.Start, Ready: app.Ready, Stop: strings.TrimSpace(app.Stop)}
		problems = requireCommand(problems, appPrefix+".start", app.Start)
		problems = requireCommand(problems, appPrefix+".ready", app.Ready)
		problems = parseDuration(problems, appPrefix+".timeouts.start", app.Timeouts.Start, 10*time.Minute, &suite.App.StartTimeout)
		problems = parseDuration(problems, appPrefix+".timeouts.ready", app.Timeouts.Ready, 5*time.Minute, &suite.App.ReadyTimeout)
		problems = parseDuration(problems, appPrefix+".timeouts.stop", app.Timeouts.Stop, 2*time.Minute, &suite.App.StopTimeout)
	}
	return suite, problems
}

// decode reads a TOML document into raw and rejects keys that raw has no
// field for.
func decode(data []byte, raw *rawRepo) (toml.MetaData, error) {
	if len(data) > MaxFileSize {
		return toml.MetaData{}, fmt.Errorf("the file has %d bytes; the limit is %d", len(data), MaxFileSize)
	}
	meta, err := toml.Decode(string(data), raw)
	if err != nil {
		// The message can quote the file, which comes from the change, so
		// it must not reach a terminal with control characters in it.
		return meta, errors.New(EscapeControl(err.Error()))
	}
	if len(meta.Keys()) == 0 {
		return meta, errors.New("the file is empty")
	}
	// A version 1 file has fields that version 2 does not know, so its
	// version decides before they do.
	if raw.Version != nil && *raw.Version == 1 {
		return meta, ErrVersion1
	}
	if unknown := unknownKeys(meta); len(unknown) > 0 {
		noun := "field"
		if len(unknown) > 1 {
			noun = "fields"
		}
		return meta, fmt.Errorf("unknown %s %s", noun, strings.Join(unknown, ", "))
	}
	// The decoder leaves a map empty, without an error, when the file gives
	// it a value that is not a table. Dotted keys, such as env.PORT, make a
	// table whose type is empty.
	if kind := meta.Type("env"); kind != "" && kind != "Hash" {
		return meta, errors.New(`env must be a table, such as env.NAME = "value"`)
	}
	return meta, nil
}

// unknownKeys lists, quoted, the keys of the file that no field of rawRepo
// has as its exact toml tag. The decoder also matches a key to a field in
// another case, so MetaData.Undecoded would let RUN set run. A key under a
// map, such as a name in env or a suite name, is not checked; the fields of
// a suite are.
func unknownKeys(meta toml.MetaData) []string {
	var unknown []string
	for _, key := range meta.Keys() {
		parent := reflect.TypeFor[rawRepo]()
		for i, part := range key {
			if parent.Kind() == reflect.Map && parent.Elem().Kind() == reflect.Struct {
				parent = parent.Elem()
				continue
			}
			if parent.Kind() != reflect.Struct {
				break
			}
			field, ok := fieldWithTag(parent, part)
			if !ok {
				// The keys under an unknown table would repeat its error, so
				// only the table's own key reports it.
				if i == len(key)-1 {
					unknown = append(unknown, strconv.Quote(key.String()))
				}
				break
			}
			parent = field.Type
		}
	}
	return unknown
}

func fieldWithTag(t reflect.Type, tag string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		if field := t.Field(i); field.Tag.Get("toml") == tag {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// Matches reports whether file, a slash-separated path from the repository
// root, matches one of the patterns in Files.
func (t Tests) Matches(file string) bool {
	name := strings.Split(file, "/")
	for _, pattern := range t.Files {
		if matchSegments(strings.Split(pattern, "/"), name) {
			return true
		}
	}
	return false
}

// matchSegments matches a path segment by segment. A ** segment matches
// any number of segments, and at the end of a pattern at least one, so that
// e2e/** matches the files in e2e but not a file named e2e.
func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		if len(pattern) == 1 {
			return len(name) > 0
		}
		for i := range len(name) {
			if matchSegments(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := path.Match(pattern[0], name[0])
	return ok && matchSegments(pattern[1:], name[1:])
}

func validPattern(pattern string) bool {
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return false
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" {
			return false
		}
		if segment == "**" {
			continue
		}
		// path.Match checks the whole pattern, even when the name does not
		// match.
		if _, err := path.Match(segment, ""); err != nil {
			return false
		}
	}
	return true
}

// isControl reports whether r is a control character or a line or paragraph
// separator, which a terminal or a viewer can act on.
func isControl(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp)
}

// EscapeControl writes every control character in s, except tabs, as a Go
// escape sequence, so that text from a change cannot act on a terminal or add
// lines to the output.
func EscapeControl(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isControl(r) && r != '\t' {
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func requireCommand(problems []string, field, command string) []string {
	if strings.TrimSpace(command) == "" {
		return append(problems, field+" is missing")
	}
	return problems
}

func checkEnvName(problems []string, field, name string) []string {
	switch {
	case !envNamePattern.MatchString(name):
		return append(problems, fmt.Sprintf("%s name %q must match [A-Za-z_][A-Za-z0-9_]*", field, name))
	case strings.HasPrefix(name, "ELPROVO_"):
		return append(problems, fmt.Sprintf("%s name %q must not start with ELPROVO_", field, name))
	}
	return problems
}

// parseDuration parses a duration from section 1.2 of SPEC.md into *out, or
// sets the default when text is empty.
func parseDuration(problems []string, field, text string, fallback time.Duration, out *time.Duration) []string {
	if text == "" {
		*out = fallback
		return problems
	}
	match := durationPattern.FindStringSubmatch(text)
	if match == nil {
		return append(problems, fmt.Sprintf("%s is %q; write a whole number followed by s, m or h, such as 90s", field, text))
	}
	count, err := strconv.Atoi(match[1])
	if err != nil {
		return append(problems, fmt.Sprintf("%s is %q; the number is too large", field, text))
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[match[2]]
	if count > int(time.Hour/unit) {
		return append(problems, fmt.Sprintf("%s is %q; the limit is 1h", field, text))
	}
	*out = time.Duration(count) * unit
	return problems
}
