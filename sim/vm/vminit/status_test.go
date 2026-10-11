package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func fakeUptime() func() float64 {
	next := 9.0
	return func() float64 { next++; return next }
}

func runStages(f *statusFile, failInStage string, warnInStage string) {
	for _, name := range stageNames {
		f.begin(name)
		if name == "init-local" {
			f.setDatasource(datasourceName)
		}
		if name == failInStage {
			f.fail("('scripts_user', 'runparts: 1 failures (runcmd) in 1 attempted commands')")
		}
		if name == warnInStage {
			f.recoverable("WARNING", "cloud-config key \"users\" is not supported")
		}
		f.finish()
	}
	f.writeResult()
}

func TestStatusIsNotStartedBeforeBoot(t *testing.T) {
	d := readStatusDetails(t.TempDir())
	if d.running != "not started" || d.exitCode() != 0 {
		t.Fatalf("details = %+v", d)
	}
}

func TestStatusIsRunningUntilResultIsWritten(t *testing.T) {
	root := t.TempDir()
	f := newStatusFile(root, fakeUptime())
	f.begin("init-local")
	f.finish()
	f.begin("init")

	d := readStatusDetails(root)
	if d.running != "running" || d.detail != "Running in stage: init" {
		t.Fatalf("details = %+v", d)
	}
}

func TestStatusExitCodesFollowErrorsAndWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, failIn, warnIn       string
		status, extended, wantJSON string
		code                       int
	}{
		{"clean boot", "", "", "done", "done", "done", 0},
		{"warning", "", "init", "done", "degraded done", "done", 2},
		{"module failure", "modules-final", "", "error", "error - done", "error", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runStages(newStatusFile(root, fakeUptime()), tc.failIn, tc.warnIn)
			var out bytes.Buffer

			code := cloudInitCommand(root, []string{"status", "--wait", "--long"}, &out, &bytes.Buffer{})
			if code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			if !strings.HasPrefix(out.String(), "status: "+tc.status+"\nextended_status: "+tc.extended+"\n") {
				t.Fatalf("output:\n%s", out.String())
			}
			out.Reset()
			cloudInitCommand(root, []string{"status", "--format", "json"}, &out, &bytes.Buffer{})
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("JSON output: %v\n%s", err, out.String())
			}
			if doc["status"] != tc.wantJSON || doc["datasource"] != "nocloud" || doc["boot_status_code"] != "enabled-by-sysvinit" {
				t.Fatalf("JSON status = %v", doc)
			}
		})
	}
}

func TestStatusLongListsErrorsAndWarningsByLevel(t *testing.T) {
	root := t.TempDir()
	runStages(newStatusFile(root, fakeUptime()), "modules-final", "init")
	var out bytes.Buffer
	cloudInitCommand(root, []string{"status", "--long"}, &out, &bytes.Buffer{})

	want := "detail: " + datasourceName + "\n" +
		"errors:\n\t- ('scripts_user', 'runparts: 1 failures (runcmd) in 1 attempted commands')\n" +
		"recoverable_errors:\nWARNING:\n\t- cloud-config key \"users\" is not supported\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Fatalf("output:\n%s\nwant suffix:\n%s", out.String(), want)
	}
}
