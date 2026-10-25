package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// minimal is the smallest valid elprovo.toml.
const minimal = "version = 2\n\n[tests]\nfiles = [\"**/*_test.go\"]\nrun = \"go test -json -count=1 {packages}\"\nreport = \"go-test-json\"\n"

func TestParseRepoReadsTheTestsAndAppliesDefaults(t *testing.T) {
	repo, err := ParseRepo("elprovo.toml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.Suites) != 1 || repo.Suites[0].Name != "tests" {
		t.Fatalf("suites are %+v; want one suite named tests", repo.Suites)
	}
	suite := repo.Suites[0]
	if len(repo.Tests.Files) != 1 || repo.Tests.Files[0] != "**/*_test.go" || suite.Run != "go test -json -count=1 {packages}" {
		t.Errorf("files are %v and run is %q; want the values of the file", repo.Tests.Files, suite.Run)
	}
	if suite.Timeout != 10*time.Minute || suite.App != nil {
		t.Errorf("timeout is %v and app is %+v; want 10m and no app", suite.Timeout, suite.App)
	}
}

func TestParseRepoReadsTheApp(t *testing.T) {
	content := "version = 2\npass_env = [\"DOCKER_HOST\"]\nenv.PORT = \"8080\"\n\n" +
		"[tests]\nfiles = [\"e2e/**\"]\nrun = \"sh e2e/run.sh {packages}\"\nreport = \"go-test-json\"\ntimeout = \"20m\"\n\n" +
		"[app]\nstart = \"docker compose up -d\"\nready = \"curl -fsS localhost:8080\"\nstop = \"docker compose down\"\ntimeouts.ready = \"1m\"\n"
	repo, err := ParseRepo("elprovo.toml", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	app := repo.Suites[0].App
	if app == nil || app.Start != "docker compose up -d" || app.Ready != "curl -fsS localhost:8080" || app.Stop != "docker compose down" {
		t.Fatalf("app is %+v; want start, ready and stop from the file", app)
	}
	if app.StartTimeout != 10*time.Minute || app.ReadyTimeout != time.Minute || app.StopTimeout != 2*time.Minute || repo.Suites[0].Timeout != 20*time.Minute {
		t.Errorf("timeouts are start %v, ready %v, stop %v, tests %v; want 10m, 1m, 2m, 20m", app.StartTimeout, app.ReadyTimeout, app.StopTimeout, repo.Suites[0].Timeout)
	}
	if repo.Env["PORT"] != "8080" || len(repo.PassEnv) != 1 {
		t.Errorf("env is %v and pass_env is %v; want PORT=8080 and DOCKER_HOST", repo.Env, repo.PassEnv)
	}
}

// suites is an elprovo.toml of version 3 with a unit suite and an e2e suite.
const suites = `version = 3

[tests]
files = ["**/*_test.go"]

[suites.unit]
run = "go test -json -count=1 {packages}"
report = "go-test-json"

[suites.e2e]
run = "go test -json -count=1 -tags=e2e ./e2e/..."
report = "go-test-json"
timeout = "20m"

[suites.e2e.app]
start = "docker compose up -d --wait"
ready = "curl -fsS localhost:8080/healthz"
stop = "docker compose down -v"
timeouts.ready = "1m"
`

func TestParseRepoReadsTheSuitesOfVersion3InFileOrder(t *testing.T) {
	repo, err := ParseRepo("elprovo.toml", []byte(strings.Replace(suites, "[suites.unit]", "[suites.zeta]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.Suites) != 2 || repo.Suites[0].Name != "zeta" || repo.Suites[1].Name != "e2e" {
		t.Fatalf("suites are %+v; want zeta, then e2e", repo.Suites)
	}
	unit, e2e := repo.Suites[0], repo.Suites[1]
	if !unit.TakesPackages() || unit.Timeout != 10*time.Minute || unit.App != nil {
		t.Errorf("the first suite is %+v; want one that takes packages, with a 10m timeout and no app", unit)
	}
	if e2e.TakesPackages() || e2e.Timeout != 20*time.Minute || e2e.Run != "go test -json -count=1 -tags=e2e ./e2e/..." {
		t.Errorf("the e2e suite is %+v; want its own command and a 20m timeout", e2e)
	}
	if e2e.App == nil || e2e.App.Start != "docker compose up -d --wait" || e2e.App.ReadyTimeout != time.Minute || e2e.App.StartTimeout != 10*time.Minute {
		t.Errorf("the e2e app is %+v; want start from the file, ready in 1m, start in 10m", e2e.App)
	}
	if len(repo.Tests.Files) != 1 {
		t.Errorf("tests.files is %v; want one pattern", repo.Tests.Files)
	}
}

func TestParseRepoRejectsInvalidVersion3Files(t *testing.T) {
	tests := []struct {
		name, content, wantInError string
	}{
		{"no suite", "version = 3\n\n[tests]\nfiles = [\"**/*_test.go\"]\n", "suites is missing"},
		{"a bad suite name", strings.Replace(suites, "[suites.unit]", "[suites.Unit]", 1), `suite name "Unit" must match [a-z0-9][a-z0-9-]*`},
		{"an unknown suite field", strings.Replace(suites, "timeout = \"20m\"", "timout = \"20m\"", 1), `unknown field "suites.e2e.timout"`},
		{"an unknown app field", strings.Replace(suites, "stop = \"docker compose down -v\"", "stp = \"x\"", 1), `unknown field "suites.e2e.app.stp"`},
		{"a suite without run", strings.Replace(suites, "run = \"go test -json -count=1 {packages}\"\n", "", 1), "suites.unit.run is missing"},
		{"another report", strings.Replace(suites, "report = \"go-test-json\"\ntimeout", "report = \"junit\"\ntimeout", 1), `suites.e2e.report is "junit"`},
		{"an app without ready", strings.Replace(suites, "ready = \"curl -fsS localhost:8080/healthz\"\n", "", 1), "suites.e2e.app.ready is missing"},
		{"tests.run in version 3", strings.Replace(suites, "files = [\"**/*_test.go\"]", "files = [\"**/*_test.go\"]\nrun = \"go test\"", 1), "tests.run is a field of version 2"},
		{"an app table in version 3", suites + "\n[app]\nstart = \"x\"\nready = \"y\"\n", "app is a table of version 2"},
		{"suites in version 2", minimal + "\n[suites.unit]\nrun = \"x\"\nreport = \"go-test-json\"\n", "suites is a table of version 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepo("elprovo.toml", []byte(tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("error is %v; want one that contains %q", err, tt.wantInError)
			}
		})
	}
}

func TestParseRepoRejectsVersion1WithItsOwnError(t *testing.T) {
	_, err := ParseRepo("elprovo.toml", []byte("version = 1\nstart = \"s\"\nready = \"r\"\n"))
	if !errors.Is(err, ErrVersion1) || !strings.Contains(err.Error(), "version 1 is the proof-file format of Elprovo 0.2") {
		t.Errorf("error is %v; want ErrVersion1 with its message", err)
	}
}

func TestParseRepoRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name, content, wantInError string
	}{
		{"missing version", strings.Replace(minimal, "version = 2\n", "", 1), "version is missing"},
		{"newer version", strings.Replace(minimal, "version = 2", "version = 4", 1), "version 4 is not supported"},
		{"missing files", strings.Replace(minimal, "files = [\"**/*_test.go\"]\n", "", 1), "tests.files is missing"},
		{"bad pattern", strings.Replace(minimal, "**/*_test.go", "[", 1), `tests.files pattern "[" is not valid`},
		{"missing run", strings.Replace(minimal, "run = \"go test -json -count=1 {packages}\"\n", "", 1), "tests.run is missing"},
		{"run without packages", strings.Replace(minimal, " {packages}", "", 1), "tests.run must contain {packages}"},
		{"missing report", strings.Replace(minimal, "report = \"go-test-json\"\n", "", 1), "tests.report is missing"},
		{"other report", strings.Replace(minimal, "go-test-json", "junit", 1), `tests.report is "junit"; this release reads go-test-json`},
		{"app without ready", minimal + "\n[app]\nstart = \"s\"\n", "app.ready is missing"},
		{"proof-file field", minimal + "start = \"s\"\n", `unknown field "tests.start"`},
		{"misspelled field", strings.Replace(minimal, "files =", "fils =", 1), `unknown field "tests.fils"`},
		{"table in capitals", strings.Replace(minimal, "[tests]", "[TESTS]", 1), `unknown field "TESTS"`},
		{"env that is not a table", "version = 2\nenv = \"PORT=8080\"\n" + strings.Replace(minimal, "version = 2\n", "", 1), "env must be a table"},
		{"env name", "version = 2\nenv.BAD-NAME = \"x\"\n" + strings.Replace(minimal, "version = 2\n", "", 1), `env name "BAD-NAME"`},
		{"reserved pass_env name", "version = 2\npass_env = [\"ELPROVO_RUN_ID\"]\n" + strings.Replace(minimal, "version = 2\n", "", 1), "must not start with ELPROVO_"},
		{"duration without unit", minimal + "timeout = \"10\"\n", `tests.timeout is "10"`},
		{"duration too long", minimal + "timeout = \"61m\"\n", "the limit is 1h"},
		{"duplicate field", minimal + "report = \"go-test-json\"\n", "'tests.report' has already been defined"},
		{"not TOML", "version: 2\n", "line 1"},
		{"empty file", "# Only a comment.\n", "the file is empty"},
		{"too large", minimal + "#" + strings.Repeat("x", MaxFileSize), "the limit is 65536"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepo("elprovo.toml", []byte(tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("error is %v; want one that contains %q", err, tt.wantInError)
			}
		})
	}
}

func TestTestsMatchFilesWithDoubleStarPatterns(t *testing.T) {
	tests := Tests{Files: []string{"**/*_test.go", "e2e/**", "cmd/*.sh"}}
	for file, want := range map[string]bool{
		"a_test.go":                 true,
		"internal/lnk/sync_test.go": true,
		"internal/lnk/sync.go":      false,
		"a_test.go.orig":            false,
		"e2e/specs/cart.ts":         true,
		"e2e":                       false,
		"cmd/run.sh":                true,
		"cmd/sub/run.sh":            false,
	} {
		if got := tests.Matches(file); got != want {
			t.Errorf("Matches(%q) = %v; want %v", file, got, want)
		}
	}
}
