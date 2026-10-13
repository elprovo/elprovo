package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	statusPath     = "/run/cloud-init/status.json"
	resultPath     = "/run/cloud-init/result.json"
	ciVersion      = "26.1-elprovo-sim"
	datasourceName = "DataSourceNoCloud [seed=/var/lib/cloud/seed/nocloud]"
)

var stageNames = []string{"init-local", "init", "modules-config", "modules-final"}

type stageStatus struct {
	Errors            []string            `json:"errors"`
	Finished          *float64            `json:"finished"`
	RecoverableErrors map[string][]string `json:"recoverable_errors"`
	Start             *float64            `json:"start"`
}

type bootStatus struct {
	datasource *string
	stage      *string
	stages     map[string]*stageStatus
}

func newBootStatus() bootStatus {
	s := bootStatus{stages: map[string]*stageStatus{}}
	for _, name := range stageNames {
		s.stages[name] = &stageStatus{Errors: []string{}, RecoverableErrors: map[string][]string{}}
	}
	return s
}

func (s bootStatus) fields() map[string]any {
	m := map[string]any{"datasource": s.datasource, "stage": s.stage}
	for name, st := range s.stages {
		m[name] = st
	}
	return m
}

func (s *bootStatus) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = newBootStatus()
	if err := json.Unmarshal(orNull(raw["datasource"]), &s.datasource); err != nil {
		return err
	}
	if err := json.Unmarshal(orNull(raw["stage"]), &s.stage); err != nil {
		return err
	}
	for _, name := range stageNames {
		if v, ok := raw[name]; ok {
			if err := json.Unmarshal(v, s.stages[name]); err != nil {
				return err
			}
		}
	}
	return nil
}

func orNull(v json.RawMessage) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	return v
}

type statusFile struct {
	root    string
	uptime  func() float64
	status  bootStatus
	current string
}

func newStatusFile(root string, uptime func() float64) *statusFile {
	_ = os.Remove(filepath.Join(root, resultPath))
	_ = os.Remove(filepath.Join(root, statusPath))
	return &statusFile{root: root, uptime: uptime, status: newBootStatus()}
}

func (f *statusFile) begin(stage string) {
	f.current = stage
	now := f.uptime()
	f.status.stage = &stage
	f.status.stages[stage].Start = &now
	f.write()
}

func (f *statusFile) finish() {
	now := f.uptime()
	f.status.stages[f.current].Finished = &now
	f.status.stage = nil
	f.write()
}

func (f *statusFile) setDatasource(name string) {
	f.status.datasource = &name
}

func (f *statusFile) recoverable(level, msg string) {
	if st, ok := f.status.stages[f.current]; ok {
		st.RecoverableErrors[level] = append(st.RecoverableErrors[level], msg)
	}
}

func (f *statusFile) fail(msg string) {
	if st, ok := f.status.stages[f.current]; ok {
		st.Errors = append(st.Errors, msg)
	}
}

func (f *statusFile) write() {
	_ = writeJSON(filepath.Join(f.root, statusPath), map[string]any{"v1": f.status.fields()})
}

func (f *statusFile) writeResult() {
	errs := []string{}
	for _, name := range stageNames {
		errs = append(errs, f.status.stages[name].Errors...)
	}
	_ = writeJSON(filepath.Join(f.root, resultPath), map[string]any{"v1": map[string]any{"datasource": f.status.datasource, "errors": errs}})
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}

func readUptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

type statusDetails struct {
	running     string
	condition   string
	bootCode    string
	datasource  any
	detail      string
	errors      []string
	levels      []string
	recoverable map[string][]string
	lastUpdate  string
	v1          bootStatus
}

func readStatusDetails(root string) statusDetails {
	d := statusDetails{running: "not started", condition: "healthy", datasource: "", errors: []string{}, recoverable: map[string][]string{}, v1: newBootStatus()}
	d.bootCode, d.detail = bootCode(root)
	data, err := os.ReadFile(filepath.Join(root, statusPath))
	if err != nil {
		return d
	}
	var file struct {
		V1 bootStatus `json:"v1"`
	}
	if json.Unmarshal(data, &file) != nil {
		return d
	}
	d.v1 = file.V1
	if _, err := os.Stat(filepath.Join(root, resultPath)); errors.Is(err, os.ErrNotExist) {
		d.running = "running"
	}
	var latest float64
	for _, name := range stageNames {
		st := d.v1.stages[name]
		d.errors = append(d.errors, st.Errors...)
		for _, level := range sortedLevels(st.RecoverableErrors) {
			if _, seen := d.recoverable[level]; !seen {
				d.levels = append(d.levels, level)
			}
			d.recoverable[level] = append(d.recoverable[level], st.RecoverableErrors[level]...)
		}
		for _, t := range []*float64{st.Start, st.Finished} {
			if t != nil {
				latest = math.Max(latest, *t)
			}
		}
	}
	if d.running == "not started" && latest > 0 {
		d.running = "done"
	}
	if latest > 0 {
		d.lastUpdate = time.Unix(int64(latest), 0).UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")
	}
	switch {
	case len(d.errors) > 0:
		d.condition = "error"
	case len(d.recoverable) > 0:
		d.condition = "degraded"
	}
	d.datasource = nil
	switch {
	case d.v1.datasource != nil:
		name := strings.ToLower(strings.Fields(*d.v1.datasource + " ")[0])
		d.datasource, d.detail = strings.Replace(name, "datasource", "", 1), *d.v1.datasource
	case d.v1.stage != nil:
		d.detail = "Running in stage: " + *d.v1.stage
	}
	return d
}

func sortedLevels(m map[string][]string) []string {
	levels := make([]string, 0, len(m))
	for level := range m {
		levels = append(levels, level)
	}
	slices.Sort(levels)
	return levels
}

func bootCode(root string) (string, string) {
	if _, err := os.Stat(filepath.Join(root, "/run/cloud-init/enabled")); err == nil {
		return "enabled-by-generator", "Cloud-init enabled by systemd cloud-init-generator"
	}
	if info, err := os.Stat(filepath.Join(root, "/run/systemd/system")); err != nil || !info.IsDir() {
		return "enabled-by-sysvinit", "Cloud-init enabled on sysvinit"
	}
	return "unknown", "Systemd generator may not have run yet."
}

func (d statusDetails) statusStrings() (string, string) {
	switch {
	case d.condition == "error":
		return "error", "error - " + d.running
	case d.condition == "degraded" && (d.running == "done" || d.running == "running"):
		return d.running, "degraded " + d.running
	}
	return d.running, d.running
}

func (d statusDetails) exitCode() int {
	switch d.condition {
	case "error":
		return 1
	case "degraded":
		return 2
	}
	return 0
}

func cloudInitCommand(root string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "clean" {
		return cleanCommand(root, args[1:], stdout, stderr)
	}
	if len(args) == 0 || args[0] != "status" {
		_, _ = fmt.Fprintln(stderr, "cloud-init: the simulated VM supports only 'cloud-init status' and 'cloud-init clean'")
		return 1
	}
	wait, long, format := false, false, "tabular"
	for i := 1; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-w" || a == "--wait":
			wait = true
		case a == "-l" || a == "--long":
			long = true
		case a == "--format" && i+1 < len(args):
			i++
			format = args[i]
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		default:
			_, _ = fmt.Fprintf(stderr, "cloud-init status: unsupported argument %s\n", a)
			return 2
		}
	}
	if format != "tabular" && format != "json" {
		_, _ = fmt.Fprintf(stderr, "cloud-init status: format %s is not supported; use tabular or json\n", format)
		return 2
	}
	d := readStatusDetails(root)
	for wait && (d.running == "not started" || d.running == "running") {
		if format == "tabular" {
			_, _ = io.WriteString(stdout, ".")
		}
		time.Sleep(250 * time.Millisecond)
		d = readStatusDetails(root)
	}
	if format == "json" {
		printStatusJSON(stdout, d)
	} else {
		printStatusText(stdout, d, long)
	}
	return d.exitCode()
}

func printStatusText(w io.Writer, d statusDetails, long bool) {
	status, extended := d.statusStrings()
	_, _ = fmt.Fprintf(w, "status: %s\n", status)
	if !long {
		return
	}
	lastUpdate := ""
	if d.lastUpdate != "" {
		lastUpdate = "last_update: " + d.lastUpdate + "\n"
	}
	errs := " []"
	if len(d.errors) > 0 {
		errs = "\n\t- " + strings.Join(d.errors, "\n\t- ")
	}
	recoverable := " {}"
	if len(d.levels) > 0 {
		var parts []string
		for _, level := range d.levels {
			msgs := make([]string, len(d.recoverable[level]))
			for i, m := range d.recoverable[level] {
				msgs[i] = strings.ReplaceAll(m, "\n", " ")
			}
			parts = append(parts, level+":\n\t- "+strings.Join(msgs, "\n\t- "))
		}
		recoverable = "\n" + strings.Join(parts, "\n")
	}
	_, _ = fmt.Fprintf(w, "extended_status: %s\nboot_status_code: %s\n%sdetail: %s\nerrors:%s\nrecoverable_errors:%s\n",
		extended, d.bootCode, lastUpdate, d.detail, errs, recoverable)
}

func printStatusJSON(w io.Writer, d statusDetails) {
	status, extended := d.statusStrings()
	out := d.v1.fields()
	delete(out, "datasource")
	for k, v := range map[string]any{
		"boot_status_code": d.bootCode, "datasource": d.datasource, "detail": d.detail, "errors": d.errors,
		"extended_status": extended, "last_update": d.lastUpdate, "recoverable_errors": d.recoverable, "status": status,
	} {
		out[k] = v
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	_, _ = fmt.Fprintf(w, "%s\n", data)
}
