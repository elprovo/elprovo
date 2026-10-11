package main

import (
	"slices"
	"strings"
	"testing"
)

func TestDecodeCloudConfigReadsTheAgentsUserData(t *testing.T) {
	cfg, notices, err := decodeCloudConfig([]byte(`{
		"ssh_deletekeys": true,
		"ssh_genkeytypes": [],
		"ssh_keys": {"ed25519_private": "PRIVATE\n", "ed25519_public": "ssh-ed25519 AAAA"},
		"package_update": true,
		"packages": ["docker.io", ["docker-compose-v2", "2.40.3"]]
	}`))
	if err != nil || len(notices) != 0 {
		t.Fatalf("decode: %v, notices %v", err, notices)
	}
	if !cfg.sshDeleteKeys || cfg.sshGenKeyTypes == nil || len(cfg.sshGenKeyTypes) != 0 {
		t.Fatalf("ssh settings = %v %v; want delete and an empty, non-nil key type list", cfg.sshDeleteKeys, cfg.sshGenKeyTypes)
	}
	if !cfg.hasSSHKeys || cfg.sshKeys["ed25519_public"] != "ssh-ed25519 AAAA" {
		t.Fatalf("ssh_keys = %v", cfg.sshKeys)
	}
	want := []packageSpec{{name: "docker.io"}, {name: "docker-compose-v2", version: "2.40.3"}}
	if !cfg.packageUpdate || !slices.Equal(cfg.packages, want) {
		t.Fatalf("packages = %v update %v", cfg.packages, cfg.packageUpdate)
	}
}

func TestDecodeCloudConfigWarnsInsteadOfIgnoringSilently(t *testing.T) {
	_, notices, err := decodeCloudConfig([]byte(`{"users": [], "runcmd": "not a list", "apt_update": true}`))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, n := range notices {
		texts = append(texts, n.level+": "+n.text)
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{
		`WARNING: Invalid cloud-config provided: runcmd:`,
		`WARNING: cloud-config key "users" is not supported`,
		`DEPRECATED: Deprecated cloud-config provided: apt_update:`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("notices lack %q:\n%s", want, joined)
		}
	}
}

func TestDecodeCloudConfigLetsRootSignInAndWarnsWhenAskedNotTo(t *testing.T) {
	if _, notices, err := decodeCloudConfig([]byte(`{"disable_root": false}`)); err != nil || len(notices) != 0 {
		t.Fatalf("disable_root false: %v, notices %v; want it accepted", err, notices)
	}
	_, notices, err := decodeCloudConfig([]byte(`{"disable_root": true}`))
	if err != nil || len(notices) != 1 || notices[0].level != "WARNING" || !strings.Contains(notices[0].text, "disable_root") {
		t.Fatalf("disable_root true: %v, notices %v; want a warning", err, notices)
	}
}

func TestDecodeCloudConfigDefaultsToDeletingHostKeys(t *testing.T) {
	cfg, _, err := decodeCloudConfig([]byte(`{}`))
	if err != nil || !cfg.sshDeleteKeys || cfg.sshGenKeyTypes != nil {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	if _, _, err := decodeCloudConfig([]byte(`["a list"]`)); err == nil {
		t.Fatal("a list decoded as a cloud-config document")
	}
}

func TestShellifyQuotesListCommandsLikeCloudInit(t *testing.T) {
	cmds, err := decodeCommands([]byte(`["echo $HOME > /tmp/h", ["sh", "-c", "echo it's", 5, true]]`))
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\necho $HOME > /tmp/h\n'sh' '-c' 'echo it'\\''s' '5' 'True'\n"
	if got := shellify(cmds); got != want {
		t.Fatalf("shellify = %q, want %q", got, want)
	}
}
