package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

type cloudConfig struct {
	sshDeleteKeys     bool
	sshGenKeyTypes    []string
	sshKeys           map[string]string
	hasSSHKeys        bool
	sshAuthorizedKeys []string
	writeFiles        []writeFile
	packages          []packageSpec
	packageUpdate     bool
	bootcmd           []command
	runcmd            []command
}

type writeFile struct {
	Path        string          `json:"path"`
	Content     string          `json:"content"`
	Encoding    string          `json:"encoding"`
	Permissions json.RawMessage `json:"permissions"`
	Owner       string          `json:"owner"`
	Append      bool            `json:"append"`
	Defer       bool            `json:"defer"`
	Source      json.RawMessage `json:"source"`
}

type packageSpec struct {
	name    string
	version string
}

type command struct {
	shell string
	args  []string
}

var defaultKeyTypes = []string{"rsa", "ecdsa", "ed25519"}

var cloudConfigKeys = map[string]func(*cloudConfig, json.RawMessage) error{
	"ssh_deletekeys": func(c *cloudConfig, v json.RawMessage) error { return json.Unmarshal(v, &c.sshDeleteKeys) },
	"ssh_genkeytypes": func(c *cloudConfig, v json.RawMessage) error {
		c.sshGenKeyTypes = []string{}
		return json.Unmarshal(v, &c.sshGenKeyTypes)
	},
	"ssh_keys": func(c *cloudConfig, v json.RawMessage) error {
		c.hasSSHKeys = true
		return json.Unmarshal(v, &c.sshKeys)
	},
	"ssh_authorized_keys": func(c *cloudConfig, v json.RawMessage) error { return json.Unmarshal(v, &c.sshAuthorizedKeys) },
	"write_files":         func(c *cloudConfig, v json.RawMessage) error { return json.Unmarshal(v, &c.writeFiles) },
	"packages":            decodePackages,
	"package_update":      func(c *cloudConfig, v json.RawMessage) error { return orBool(&c.packageUpdate, v) },
	"apt_update":          func(c *cloudConfig, v json.RawMessage) error { return orBool(&c.packageUpdate, v) },
	"bootcmd":             func(c *cloudConfig, v json.RawMessage) (err error) { c.bootcmd, err = decodeCommands(v); return err },
	"runcmd":              func(c *cloudConfig, v json.RawMessage) (err error) { c.runcmd, err = decodeCommands(v); return err },
	"disable_root":        decodeDisableRoot,
}

// decodeDisableRoot accepts disable_root false, which is how the simulated
// VM always behaves: root signs in with the instance's keys.
func decodeDisableRoot(_ *cloudConfig, v json.RawMessage) error {
	var disable bool
	if err := json.Unmarshal(v, &disable); err != nil {
		return err
	}
	if disable {
		return errors.New("true is not supported by the simulated VM, which has no default user; root keeps its keys")
	}
	return nil
}

var deprecatedKeys = map[string]string{
	"apt_update": "Deprecated in version 22.2. Use ``package_update`` instead.",
}

func decodeCloudConfig(doc []byte) (cloudConfig, []notice, error) {
	cfg := cloudConfig{sshDeleteKeys: true}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return cfg, nil, errors.New("cloud-config is not a mapping of keys to values")
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var notices []notice
	for _, key := range keys {
		decode, ok := cloudConfigKeys[key]
		if !ok {
			notices = append(notices, warning("cloud-config key %q is not supported by the simulated VM; it was ignored", key))
			continue
		}
		if err := decode(&cfg, top[key]); err != nil {
			notices = append(notices, warning("Invalid cloud-config provided: %s: %v; it was ignored", key, err))
		}
		if text, ok := deprecatedKeys[key]; ok {
			notices = append(notices, notice{level: "DEPRECATED", text: "Deprecated cloud-config provided: " + key + ":  " + text})
		}
	}
	return cfg, notices, nil
}

func (c cloudConfig) hasWriteFiles(deferred bool) bool {
	return slices.ContainsFunc(c.writeFiles, func(f writeFile) bool { return f.Defer == deferred })
}

func orBool(dst *bool, v json.RawMessage) error {
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return err
	}
	*dst = *dst || b
	return nil
}

func decodePackages(c *cloudConfig, v json.RawMessage) error {
	var items []json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil {
		return err
	}
	for _, item := range items {
		var name string
		if json.Unmarshal(item, &name) == nil {
			c.packages = append(c.packages, packageSpec{name: name})
			continue
		}
		var pair []string
		if err := json.Unmarshal(item, &pair); err != nil || len(pair) != 2 {
			return fmt.Errorf("package %s is neither a name nor a [name, version] pair", item)
		}
		c.packages = append(c.packages, packageSpec{name: pair[0], version: pair[1]})
	}
	return nil
}

func decodeCommands(v json.RawMessage) ([]command, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, err
	}
	cmds := make([]command, 0, len(items))
	for _, item := range items {
		var line string
		if json.Unmarshal(item, &line) == nil {
			cmds = append(cmds, command{shell: line})
			continue
		}
		var args []any
		if err := json.Unmarshal(item, &args); err != nil {
			return nil, fmt.Errorf("command %s is neither a string nor a list", item)
		}
		cmd := command{args: make([]string, len(args))}
		for i, a := range args {
			cmd.args[i] = scalarString(a)
		}
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

func scalarString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	default:
		return fmt.Sprint(v)
	}
}

func shellify(cmds []command) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, c := range cmds {
		if c.args == nil {
			b.WriteString(c.shell + "\n")
			continue
		}
		quoted := make([]string, len(c.args))
		for i, a := range c.args {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		b.WriteString(strings.Join(quoted, " ") + "\n")
	}
	return b.String()
}

const yamlToJSONScript = `import datetime, json, sys, yaml
def plain(value):
    if isinstance(value, (datetime.date, datetime.datetime)):
        return value.isoformat()
    raise TypeError("YAML value of type %s is not supported" % type(value).__name__)
json.dump(yaml.safe_load(sys.stdin), sys.stdout, default=plain)
`

func yamlToJSON(ctx context.Context, doc []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "python3", "-I", "-c", yamlToJSONScript)
	cmd.Stdin = bytes.NewReader(doc)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w: %s", err, lastLine(stderr.String()))
	}
	return out, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
