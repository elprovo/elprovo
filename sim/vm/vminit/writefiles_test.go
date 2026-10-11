package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func TestDecodeContentHandlesCloudInitEncodings(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("hello"))
	_ = w.Close()
	for _, tc := range []struct {
		encoding, content string
	}{
		{"", "hello"},
		{"text/plain", "hello"},
		{"b64", base64.StdEncoding.EncodeToString([]byte("hello"))},
		{"base64", base64.StdEncoding.EncodeToString([]byte("hello"))},
		{"gzip", gz.String()},
		{"gz+b64", base64.StdEncoding.EncodeToString(gz.Bytes())},
		{"gzip+base64", base64.StdEncoding.EncodeToString(gz.Bytes())},
	} {
		got, known, err := decodeContent(tc.content, tc.encoding)
		if err != nil || !known || string(got) != "hello" {
			t.Errorf("encoding %q: got %q known %v err %v", tc.encoding, got, known, err)
		}
	}
}

func TestParsePermissionsAcceptsOctalStringsAndYAMLNumbers(t *testing.T) {
	for raw, want := range map[string]os.FileMode{
		`"0600"`:  0o600,
		`"0o755"`: 0o755,
		`"644"`:   0o644,
		`420`:     0o644,
		``:        0o644,
	} {
		got, err := parsePermissions(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("parsePermissions(%s) = %o, %v; want %o", raw, got, err, want)
		}
	}
}

func TestPlanWriteFilesWarnsAndFallsBackLikeCloudInit(t *testing.T) {
	files := []writeFile{
		{Path: "etc/app.conf", Content: "x", Encoding: "rot13", Permissions: json.RawMessage(`"rw-r--r--"`)},
		{Path: "/late", Content: "y", Defer: true},
	}
	plan, notices, err := planWriteFiles(files, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].path != "/etc/app.conf" || plan[0].mode != 0o644 || plan[0].owner != "root:root" || string(plan[0].data) != "x" {
		t.Fatalf("plan = %+v; want only the non-deferred file, made absolute, as plain text with 0644 root:root", plan)
	}
	if len(notices) != 2 {
		t.Fatalf("notices = %v, want one for the encoding and one for the permissions", notices)
	}
	deferred, _, _ := planWriteFiles(files, true)
	if len(deferred) != 1 || deferred[0].path != "/late" {
		t.Fatalf("deferred plan = %+v", deferred)
	}
}

func TestPlanWriteFilesRejectsRemoteSources(t *testing.T) {
	_, _, err := planWriteFiles([]writeFile{{Path: "/x", Source: json.RawMessage(`{"uri": "https://example.com/x"}`)}}, false)
	if err == nil {
		t.Fatal("a source entry was accepted")
	}
}
