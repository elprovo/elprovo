package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const unitDir = "/etc/systemd/system"

type unit struct {
	name             string
	path             string
	description      string
	serviceType      string
	execStartPre     []execLine
	execStart        []execLine
	environment      []string
	environmentFiles []string
	workingDirectory string
	// stateDirectories are names under /var/lib that the service owns.
	stateDirectories   []string
	stateDirectoryMode os.FileMode
	// ambientCaps are the capabilities the service's processes keep when
	// they run as an unprivileged user.
	ambientCaps     []uintptr
	user            string
	group           string
	restart         string
	restartDelay    time.Duration
	remainAfterExit bool
	wantedBy        []string
	warnings        []string
}

type execLine struct {
	raw           string
	ignoreFailure bool
}

var orderingKeys = map[string]bool{
	"After": true, "Before": true, "Wants": true, "Requires": true, "BindsTo": true,
	"PartOf": true, "Conflicts": true, "Documentation": true,
}

func normalizeUnitName(name string) (string, error) {
	if strings.ContainsRune(name, '/') || name == "" {
		return "", fmt.Errorf("invalid unit name %q", name)
	}
	if !strings.Contains(name, ".") {
		return name + ".service", nil
	}
	if !strings.HasSuffix(name, ".service") {
		return "", fmt.Errorf("unit %s: only .service units are supported", name)
	}
	return name, nil
}

func loadUnit(root, name string) (unit, error) {
	path := filepath.Join(unitDir, name)
	f, err := os.Open(filepath.Join(root, path))
	if err != nil {
		return unit{}, err
	}
	defer func() { _ = f.Close() }()
	u, err := parseUnit(name, f)
	u.path = path
	return u, err
}

func parseUnit(name string, r io.Reader) (unit, error) {
	u := unit{name: name, serviceType: "simple", restart: "no", restartDelay: 100 * time.Millisecond, stateDirectoryMode: 0o755}
	section := ""
	lines, err := unitLines(r)
	if err != nil {
		return u, err
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			u.warnings = append(u.warnings, fmt.Sprintf("Missing '=' in line %q, ignoring.", line))
			continue
		}
		if err := u.set(section, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return u, fmt.Errorf("unit %s: %w", name, err)
		}
	}
	if len(u.execStart) == 0 {
		return u, fmt.Errorf("unit %s has no ExecStart= setting", name)
	}
	if u.serviceType != "oneshot" && len(u.execStart) > 1 {
		return u, fmt.Errorf("unit %s has more than one ExecStart= setting, which is only allowed for Type=oneshot", name)
	}
	if u.serviceType == "oneshot" && u.restart == "always" {
		return u, fmt.Errorf("unit %s: Restart=always is not allowed for Type=oneshot", name)
	}
	return u, nil
}

func unitLines(r io.Reader) ([]string, error) {
	var lines []string
	var pending strings.Builder
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if pending.Len() == 0 && (line == "" || line[0] == '#' || line[0] == ';') {
			continue
		}
		if strings.HasSuffix(line, `\`) {
			pending.WriteString(strings.TrimSpace(strings.TrimSuffix(line, `\`)) + " ")
			continue
		}
		pending.WriteString(line)
		lines = append(lines, pending.String())
		pending.Reset()
	}
	if pending.Len() > 0 {
		lines = append(lines, pending.String())
	}
	return lines, sc.Err()
}

func (u *unit) set(section, key, value string) error {
	switch section {
	case "Unit":
		return u.setUnit(key, value)
	case "Service":
		return u.setService(key, value)
	case "Install":
		return u.setInstall(key, value)
	default:
		u.warnings = append(u.warnings, fmt.Sprintf("Unknown section '%s'. Ignoring.", section))
		return nil
	}
}

func (u *unit) setUnit(key, value string) error {
	switch {
	case key == "Description":
		u.description = value
	case orderingKeys[key]:
	default:
		u.unsupported("Unit", key)
	}
	return nil
}

func (u *unit) setService(key, value string) error {
	switch key {
	case "Type":
		switch value {
		case "simple", "exec", "oneshot", "notify":
			u.serviceType = value
		default:
			return fmt.Errorf("unsupported Type=%s; use simple, exec, oneshot or notify", value)
		}
	case "ExecStart":
		return appendExec(&u.execStart, value)
	case "ExecStartPre":
		return appendExec(&u.execStartPre, value)
	case "Environment":
		if value == "" {
			u.environment = nil
			return nil
		}
		words, err := splitWords(value)
		if err != nil {
			return fmt.Errorf("environment %q: %w", value, err)
		}
		u.environment = append(u.environment, words...)
	case "EnvironmentFile":
		if value == "" {
			u.environmentFiles = nil
			return nil
		}
		u.environmentFiles = append(u.environmentFiles, value)
	case "WorkingDirectory":
		u.workingDirectory = value
	case "StateDirectory":
		for _, dir := range strings.Fields(value) {
			if filepath.IsAbs(dir) || dir != filepath.Clean(dir) || strings.HasPrefix(dir, "..") {
				return fmt.Errorf("StateDirectory=%s must be a relative path under /var/lib", dir)
			}
			u.stateDirectories = append(u.stateDirectories, dir)
		}
	case "AmbientCapabilities":
		caps, err := parseCapabilities(value)
		if err != nil {
			return fmt.Errorf("AmbientCapabilities=%s: %w", value, err)
		}
		u.ambientCaps = append(u.ambientCaps, caps...)
	case "StateDirectoryMode":
		mode, err := strconv.ParseUint(value, 8, 32)
		if err != nil || mode > 0o7777 {
			return fmt.Errorf("StateDirectoryMode=%s is not an octal file mode", value)
		}
		u.stateDirectoryMode = os.FileMode(mode)
	case "User":
		u.user = value
	case "Group":
		u.group = value
	case "Restart":
		switch value {
		case "no", "always", "on-failure":
			u.restart = value
		default:
			return fmt.Errorf("unsupported Restart=%s; use no, always or on-failure", value)
		}
	case "RestartSec":
		d, err := parseSeconds(value)
		if err != nil {
			return fmt.Errorf("restart delay %q: %w", value, err)
		}
		u.restartDelay = d
	case "RemainAfterExit":
		u.remainAfterExit = parseBool(value)
	default:
		u.unsupported("Service", key)
	}
	return nil
}

func (u *unit) setInstall(key, value string) error {
	if key != "WantedBy" {
		u.unsupported("Install", key)
		return nil
	}
	u.wantedBy = append(u.wantedBy, strings.Fields(value)...)
	return nil
}

func (u *unit) unsupported(section, key string) {
	u.warnings = append(u.warnings, fmt.Sprintf("%s= in section [%s] is not supported by stagely-vm-init, ignoring.", key, section))
}

func appendExec(list *[]execLine, value string) error {
	if value == "" {
		*list = nil
		return nil
	}
	line := execLine{raw: value}
	for line.raw != "" && strings.ContainsRune("-@:+!", rune(line.raw[0])) {
		if line.raw[0] != '-' {
			return fmt.Errorf("command prefix %q in %q is not supported", line.raw[0], value)
		}
		line.ignoreFailure = true
		line.raw = line.raw[1:]
	}
	*list = append(*list, line)
	return nil
}

func (l execLine) argv(env []string) ([]string, error) {
	words, err := splitWords(l.raw)
	if err != nil {
		return nil, err
	}
	lookup := envMap(env)
	var argv []string
	for _, w := range words {
		if name, ok := strings.CutPrefix(w, "$"); ok && isEnvName(name) {
			argv = append(argv, strings.Fields(lookup[name])...)
			continue
		}
		argv = append(argv, expandBraces(w, lookup))
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command line")
	}
	return argv, nil
}

func expandBraces(word string, vars map[string]string) string {
	var b strings.Builder
	for {
		start := strings.Index(word, "${")
		if start < 0 {
			break
		}
		end := strings.IndexByte(word[start:], '}')
		if end < 0 {
			break
		}
		b.WriteString(word[:start])
		b.WriteString(vars[word[start+2:start+end]])
		word = word[start+end+1:]
	}
	b.WriteString(word)
	return b.String()
}

func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unbalanced quoting in %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func readEnvironmentFile(spec string) ([]string, error) {
	path, optional := strings.CutPrefix(spec, "-")
	data, err := os.ReadFile(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var env []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		env = append(env, strings.TrimSpace(k)+"="+v)
	}
	return env, nil
}

func parseSeconds(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	return time.ParseDuration(s + "s")
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "1", "yes", "true", "on":
		return true
	}
	return false
}
