package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseUnitReadsSupportedDirectives(t *testing.T) {
	u, err := parseUnit("app.service", strings.NewReader(`
[Unit]
Description=App server
After=network-online.target

[Service]
Type=notify
ExecStartPre=-/bin/mkdir -p /run/app
ExecStart=/usr/bin/app \
  --port 8080
Environment="GREETING=hello world" MODE=prod
EnvironmentFile=-/etc/default/app
WorkingDirectory=/srv/app
User=app
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`))
	if err != nil {
		t.Fatalf("parseUnit: %v", err)
	}
	if u.description != "App server" || u.serviceType != "notify" || u.restart != "on-failure" || u.restartDelay != 2*time.Second {
		t.Fatalf("unit = %+v", u)
	}
	if len(u.execStartPre) != 1 || !u.execStartPre[0].ignoreFailure || u.execStartPre[0].raw != "/bin/mkdir -p /run/app" {
		t.Fatalf("ExecStartPre = %+v, want one line that ignores failure", u.execStartPre)
	}
	if u.execStart[0].raw != "/usr/bin/app --port 8080" {
		t.Fatalf("ExecStart = %q, want the continuation line joined", u.execStart[0].raw)
	}
	if !slices.Equal(u.environment, []string{"GREETING=hello world", "MODE=prod"}) {
		t.Fatalf("Environment = %q", u.environment)
	}
	if u.workingDirectory != "/srv/app" || u.user != "app" || !slices.Equal(u.wantedBy, []string{"multi-user.target"}) {
		t.Fatalf("unit = %+v", u)
	}
	if len(u.warnings) != 0 {
		t.Fatalf("warnings = %q, want none: After= is accepted without effect", u.warnings)
	}
}

func TestParseUnitWarnsAboutDirectivesItIgnores(t *testing.T) {
	u, err := parseUnit("app.service", strings.NewReader("[Service]\nExecStart=/bin/true\nLimitNOFILE=4096\n"))
	if err != nil {
		t.Fatalf("parseUnit: %v", err)
	}
	if len(u.warnings) != 1 || !strings.Contains(u.warnings[0], "LimitNOFILE=") {
		t.Fatalf("warnings = %q, want one about LimitNOFILE=", u.warnings)
	}
}

func TestParseUnitRejectsWhatItCannotRun(t *testing.T) {
	for name, text := range map[string]string{
		"no ExecStart":          "[Service]\nType=simple\n",
		"forking service":       "[Service]\nType=forking\nExecStart=/usr/sbin/nginx\n",
		"two main commands":     "[Service]\nExecStart=/bin/a\nExecStart=/bin/b\n",
		"restart on abort":      "[Service]\nExecStart=/bin/a\nRestart=on-abort\n",
		"oneshot always":        "[Service]\nType=oneshot\nExecStart=/bin/a\nRestart=always\n",
		"privileged prefix (+)": "[Service]\nExecStart=+/bin/a\n",
		"absolute state dir":    "[Service]\nExecStart=/bin/a\nStateDirectory=/etc\n",
		"state dir escapes":     "[Service]\nExecStart=/bin/a\nStateDirectory=../etc\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUnit("app.service", strings.NewReader(text)); err == nil {
				t.Fatal("parseUnit succeeded, want an error")
			}
		})
	}
}

func TestExecLineExpandsVariablesLikeSystemd(t *testing.T) {
	line := execLine{raw: `/bin/app $FLAGS "--name=${NAME}" --cost=$5`}
	argv, err := line.argv([]string{"FLAGS=-a -b", "NAME=web 1"})
	if err != nil {
		t.Fatalf("argv: %v", err)
	}
	want := []string{"/bin/app", "-a", "-b", "--name=web 1", "--cost=$5"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
}

func TestNormalizeUnitNameAddsServiceSuffix(t *testing.T) {
	if got, err := normalizeUnitName("docker"); err != nil || got != "docker.service" {
		t.Fatalf("normalizeUnitName(docker) = %q, %v", got, err)
	}
	if _, err := normalizeUnitName("app.timer"); err == nil {
		t.Fatal("normalizeUnitName accepted a timer unit")
	}
}

func TestReadEnvironmentFileSkipsCommentsAndQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("# comment\nA=1\nB=\"two words\"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := readEnvironmentFile(path)
	if err != nil || !slices.Equal(env, []string{"A=1", "B=two words"}) {
		t.Fatalf("readEnvironmentFile = %q, %v", env, err)
	}
	if env, err := readEnvironmentFile("-" + path + ".missing"); err != nil || env != nil {
		t.Fatalf("optional missing file = %q, %v; want nothing and no error", env, err)
	}
	if _, err := readEnvironmentFile(path + ".missing"); err == nil {
		t.Fatal("a required missing file gave no error")
	}
}
