package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const seedDir = "/var/lib/cloud/seed/nocloud"

type metaData struct {
	InstanceID    string          `json:"instance-id"`
	LocalHostname string          `json:"local-hostname"`
	LocalIPv4     string          `json:"local-ipv4"`
	PublicIPv4    string          `json:"public-ipv4"`
	PublicKeys    json.RawMessage `json:"public-keys"`
}

func (m metaData) keys() ([]string, error) {
	if len(m.PublicKeys) == 0 || string(m.PublicKeys) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(m.PublicKeys, &one) == nil {
		return splitLines(one), nil
	}
	var many []string
	if err := json.Unmarshal(m.PublicKeys, &many); err != nil {
		return nil, errors.New("public-keys is neither a string nor a list of strings")
	}
	return many, nil
}

type userData struct {
	cloudConfig []byte
	script      []byte
	notices     []notice
}

func readSeed(ctx context.Context, root string) (metaData, userData, error) {
	var meta metaData
	raw, err := os.ReadFile(filepath.Join(root, seedDir, "meta-data"))
	if err != nil {
		return meta, userData{}, fmt.Errorf("read meta-data: %w", err)
	}
	doc, err := yamlToJSON(ctx, raw)
	if err == nil {
		err = json.Unmarshal(doc, &meta)
	}
	if err != nil {
		return meta, userData{}, fmt.Errorf("meta-data: %w", err)
	}
	if meta.InstanceID == "" {
		return meta, userData{}, errors.New("meta-data has no instance-id")
	}
	raw, err = os.ReadFile(filepath.Join(root, seedDir, "user-data"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return meta, userData{}, fmt.Errorf("read user-data: %w", err)
	}
	return meta, classifyUserData(ctx, raw), nil
}

func classifyUserData(ctx context.Context, raw []byte) userData {
	head := bytes.ToLower(bytes.TrimLeft(raw, " \t\r\n"))
	switch {
	case len(head) == 0:
		return userData{}
	case bytes.HasPrefix(head, []byte("#cloud-config")) && !bytes.HasPrefix(head, []byte("#cloud-config-")):
		doc, err := yamlToJSON(ctx, raw)
		if err != nil {
			return userData{notices: []notice{warning("Failed loading yaml blob. %v", err)}}
		}
		if string(bytes.TrimSpace(doc)) == "null" {
			return userData{notices: []notice{warning("Failed at merging in cloud config part from part-001: empty cloud config")}}
		}
		return userData{cloudConfig: doc}
	case bytes.HasPrefix(head, []byte("#!")):
		return userData{script: raw}
	default:
		first, _, _ := strings.Cut(string(bytes.TrimLeft(raw, " \t\r\n")), "\n")
		return userData{notices: []notice{warning("Unhandled user-data starting with %q: the simulated VM runs only "+
			"#cloud-config documents and #! scripts, not MIME multipart, gzip, #include or jinja templates", first)}}
	}
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
