package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type fileWrite struct {
	path   string
	data   []byte
	mode   os.FileMode
	owner  string
	append bool
}

func planWriteFiles(files []writeFile, deferred bool) ([]fileWrite, []notice, error) {
	var plan []fileWrite
	var notices []notice
	for i, f := range files {
		if f.Defer != deferred {
			continue
		}
		w, fileNotices, err := planWriteFile(f)
		notices = append(notices, fileNotices...)
		if err != nil {
			return nil, notices, fmt.Errorf("write_files entry %d (%s): %w", i, f.Path, err)
		}
		plan = append(plan, w)
	}
	return plan, notices, nil
}

func planWriteFile(f writeFile) (fileWrite, []notice, error) {
	if f.Path == "" {
		return fileWrite{}, nil, errors.New("path is required")
	}
	if len(f.Source) > 0 && string(f.Source) != "null" {
		return fileWrite{}, nil, errors.New("source is not supported by the simulated VM; give content instead")
	}
	var notices []notice
	data, known, err := decodeContent(f.Content, f.Encoding)
	if err != nil {
		return fileWrite{}, nil, err
	}
	if !known {
		notices = append(notices, warning("Unknown encoding type %s, assuming text/plain", f.Encoding))
	}
	mode, err := parsePermissions(f.Permissions)
	if err != nil {
		notices = append(notices, warning("Undecodable permissions %s, returning default 0o644", f.Permissions))
		mode = 0o644
	}
	owner := f.Owner
	if owner == "" {
		owner = "root:root"
	}
	return fileWrite{path: filepath.Join("/", f.Path), data: data, mode: mode, owner: owner, append: f.Append}, notices, nil
}

func decodeContent(content, encoding string) (data []byte, known bool, err error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "text/plain":
		return []byte(content), true, nil
	case "b64", "base64":
		data, err = base64.StdEncoding.DecodeString(content)
	case "gz", "gzip":
		data, err = gunzip([]byte(content))
	case "gz+b64", "gz+base64", "gzip+b64", "gzip+base64":
		var raw []byte
		if raw, err = base64.StdEncoding.DecodeString(content); err == nil {
			data, err = gunzip(raw)
		}
	default:
		return []byte(content), false, nil
	}
	return data, true, err
}

func gunzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func parsePermissions(raw json.RawMessage) (os.FileMode, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0o644, nil
	}
	var n uint64
	if json.Unmarshal(raw, &n) == nil {
		return os.FileMode(n) & os.ModePerm, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("permissions %s are neither an octal string nor a number", raw)
	}
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0o"), "0O")
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("permissions %q are not octal", s)
	}
	return os.FileMode(n) & os.ModePerm, nil
}

func writeFileEntry(root string, w fileWrite) error {
	path := filepath.Join(root, w.path)
	uid, gid, err := lookupOwner(w.owner)
	if err != nil {
		return err
	}
	if err := makeParents(filepath.Dir(path), uid, gid); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if w.append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, w.mode)
	if err != nil {
		return err
	}
	_, err = f.Write(w.data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(path, w.mode); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func makeParents(dir string, uid, gid int) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := makeParents(filepath.Dir(dir), uid, gid); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chown(dir, uid, gid)
}

func lookupOwner(owner string) (int, int, error) {
	name, group, hasGroup := strings.Cut(owner, ":")
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	if !hasGroup {
		return uid, -1, nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, 0, err
	}
	gid, _ := strconv.Atoi(g.Gid)
	return uid, gid, nil
}
