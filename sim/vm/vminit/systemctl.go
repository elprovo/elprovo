package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const runDir = "/run/stagely-vm-init"

var bootTargets = []string{"multi-user.target", "default.target"}

type controlRequest struct {
	Verb  string   `json:"verb"`
	Units []string `json:"units"`
}

type controlResponse struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type missingUnitError struct{ name, state string }

func (e missingUnitError) Error() string { return "Unit " + e.name + " not " + e.state + "." }

type startFailedError struct{ name string }

func (e startFailedError) Error() string {
	return fmt.Sprintf("Job for %s failed because the control process exited with error code.\n"+
		"See \"systemctl status %s\" and \"journalctl -u %s\" for details.", e.name, e.name, e.name)
}

func (s *supervisor) serveControl(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.answer(conn)
	}
}

func (s *supervisor) answer(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var req controlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	_ = json.NewEncoder(conn).Encode(s.handle(req))
}

func (s *supervisor) handle(req controlRequest) controlResponse {
	var resp controlResponse
	switch req.Verb {
	case "daemon-reload":
		if err := s.reload(); err != nil {
			resp.Code, resp.Stderr = 1, err.Error()+"\n"
		}
	case "start", "stop", "restart":
		for _, name := range req.Units {
			if err := s.act(req.Verb, name); err != nil {
				resp.Code, resp.Stderr = failureCode(err), resp.Stderr+failureText(req.Verb, name, err)
			}
		}
	case "status":
		for _, name := range req.Units {
			out, errText, code := s.status(name)
			resp.Stdout += out
			resp.Stderr += errText
			resp.Code = max(resp.Code, code)
		}
	case "is-active":
		resp.Code = 3
		for _, name := range req.Units {
			snap, _ := s.snapshot(name)
			state := snap.state
			if state == "" {
				state = "inactive"
			}
			resp.Stdout += state + "\n"
			if state == "active" {
				resp.Code = 0
			}
		}
	default:
		resp.Code, resp.Stderr = 1, fmt.Sprintf("Unknown command verb '%s'.\n", req.Verb)
	}
	return resp
}

func (s *supervisor) act(verb, name string) error {
	switch verb {
	case "start":
		return s.start(name)
	case "stop":
		return s.stop(name)
	default:
		return s.restart(name)
	}
}

func failureCode(err error) int {
	if errors.As(err, new(missingUnitError)) {
		return 5
	}
	return 1
}

func failureText(verb, name string, err error) string {
	if errors.As(err, new(startFailedError)) {
		return err.Error() + "\n"
	}
	return fmt.Sprintf("Failed to %s %s: %v\n", verb, name, err)
}

func (s *supervisor) status(name string) (stdout, stderr string, code int) {
	snap, loaded := s.snapshot(name)
	if !loaded {
		def, err := loadUnit(s.root, name)
		if err != nil {
			return "", fmt.Sprintf("Unit %s could not be found.\n", name), 4
		}
		snap = service{def: def, state: "inactive", sub: "dead"}
	}
	var b strings.Builder
	mark := map[string]string{"active": "●", "failed": "×"}[snap.state]
	if mark == "" {
		mark = "○"
	}
	fmt.Fprintf(&b, "%s %s\n", mark, describe(snap.def))
	fmt.Fprintf(&b, "     Loaded: loaded (%s; %s; preset: enabled)\n", snap.def.path, enablement(s.root, snap.def))
	fmt.Fprintf(&b, "     Active: %s (%s)", snap.state, snap.sub)
	if !snap.since.IsZero() {
		fmt.Fprintf(&b, " since %s; %s ago", snap.since.Format("Mon 2006-01-02 15:04:05 MST"), time.Since(snap.since).Round(time.Second))
	}
	b.WriteString("\n")
	if snap.pid > 0 {
		fmt.Fprintf(&b, "   Main PID: %d\n", snap.pid)
	}
	if data, err := os.ReadFile(filepath.Join(s.journal.dir, name+".log")); err == nil && len(data) > 0 {
		b.WriteString("\n" + lastLines(string(data), 10))
	}
	if snap.state != "active" {
		code = 3
	}
	return b.String(), "", code
}

func enablement(root string, def unit) string {
	if len(def.wantedBy) == 0 {
		return "static"
	}
	for _, target := range def.wantedBy {
		if _, err := os.Lstat(filepath.Join(root, unitDir, target+".wants", def.name)); err == nil {
			return "enabled"
		}
	}
	return "disabled"
}

func enabledUnits(root string) []string {
	var names []string
	for _, target := range bootTargets {
		entries, _ := os.ReadDir(filepath.Join(root, unitDir, target+".wants"))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".service") && !slices.Contains(names, e.Name()) {
				names = append(names, e.Name())
			}
		}
	}
	return names
}

func enableUnit(root, name string, out io.Writer) error {
	def, err := loadUnit(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unit file %s does not exist", name)
	}
	if err != nil {
		return err
	}
	if len(def.wantedBy) == 0 {
		_, _ = fmt.Fprintf(out, "The unit files have no installation config (WantedBy=, RequiredBy=, Also=,\n"+
			"Alias= settings in the [Install] section). This means they are not meant to be\nenabled using systemctl.\n")
		return nil
	}
	target := filepath.Join(unitDir, name)
	for _, wanted := range def.wantedBy {
		link := filepath.Join(unitDir, wanted+".wants", name)
		if _, err := os.Lstat(filepath.Join(root, link)); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(link)), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "Created symlink %s → %s.\n", link, target)
	}
	return nil
}

func disableUnit(root, name string, out io.Writer) error {
	links, err := filepath.Glob(filepath.Join(root, unitDir, "*.wants", name))
	if err != nil {
		return err
	}
	for _, link := range links {
		if err := os.Remove(link); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, link)
		_, _ = fmt.Fprintf(out, "Removed \"%s\".\n", filepath.Join("/", rel))
	}
	return nil
}

func systemctlCommand(root, socket string, args []string, stdout, stderr io.Writer) int {
	var verb string
	var units []string
	now, quiet := false, false
	for _, a := range args {
		switch {
		case a == "--now":
			now = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--no-pager" || a == "--system" || a == "--no-block" || a == "--full" || a == "-l":
		case strings.HasPrefix(a, "-"):
			_, _ = fmt.Fprintf(stderr, "systemctl: option %s is not supported\n", a)
			return 1
		case verb == "":
			verb = a
		default:
			name, err := normalizeUnitName(a)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, "systemctl:", err)
				return 1
			}
			units = append(units, name)
		}
	}
	if quiet {
		stdout = io.Discard
	}
	switch verb {
	case "enable", "disable":
		return changeEnablement(root, socket, verb, units, now, stdout, stderr)
	case "daemon-reload", "start", "stop", "restart", "status", "is-active":
		if verb != "daemon-reload" && len(units) == 0 {
			_, _ = fmt.Fprintf(stderr, "Too few arguments.\n")
			return 1
		}
		return askSupervisor(socket, controlRequest{Verb: verb, Units: units}, stdout, stderr)
	case "":
		_, _ = fmt.Fprintln(stderr, "systemctl: listing units is not supported; name a verb")
		return 1
	default:
		_, _ = fmt.Fprintf(stderr, "Unknown command verb '%s'.\n", verb)
		return 1
	}
}

func changeEnablement(root, socket, verb string, units []string, now bool, stdout, stderr io.Writer) int {
	change, then := enableUnit, "start"
	if verb == "disable" {
		change, then = disableUnit, "stop"
	}
	for _, name := range units {
		if err := change(root, name, stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "Failed to %s unit: %v\n", verb, err)
			return 1
		}
	}
	if !now {
		return 0
	}
	return askSupervisor(socket, controlRequest{Verb: then, Units: units}, stdout, stderr)
}

func askSupervisor(socket string, req controlRequest, stdout, stderr io.Writer) int {
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", socket)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "systemctl: the stagely-vm-init supervisor is not running: %v\n", err)
		return 1
	}
	defer func() { _ = conn.Close() }()
	var resp controlResponse
	err = json.NewEncoder(conn).Encode(req)
	if err == nil {
		err = json.NewDecoder(conn).Decode(&resp)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "systemctl: %v\n", err)
		return 1
	}
	_, _ = io.WriteString(stdout, resp.Stdout)
	_, _ = io.WriteString(stderr, resp.Stderr)
	return resp.Code
}
