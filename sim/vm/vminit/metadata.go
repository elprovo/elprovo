package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A cloud serves each machine's instance metadata at 169.254.169.254, from
// outside the machine. The simulated machine runs the server in its own
// network namespace behind a veth pair, so the machine routes requests to it
// as to a neighbor: requests from containers cross Docker's FORWARD chain, as
// they do on a cloud VM.
const (
	metadataFile      = "/var/lib/stagely-vm/metadata.json"
	metadataNamespace = "stagely-metadata"
	metadataAddress   = "169.254.169.254"
	// metadataPeer is the machine's end of the veth pair, which the server
	// sends its answers to.
	metadataPeer = "169.254.0.1"
)

// metadataNetCommand creates the server's network namespace and the veth
// pair that joins it to the machine. The machine's network is new at every
// boot, so it builds both then; when the server restarts, they exist already.
func metadataNetCommand(ctx context.Context, stderr io.Writer) int {
	if exec.CommandContext(ctx, "ip", "link", "show", "md0").Run() == nil {
		return 0
	}
	steps := [][]string{
		{"ip", "netns", "add", metadataNamespace},
		{"ip", "link", "add", "md0", "type", "veth", "peer", "name", "md1"},
		{"ip", "link", "set", "md1", "netns", metadataNamespace},
		{"ip", "addr", "add", metadataPeer + "/32", "dev", "md0"},
		{"ip", "link", "set", "md0", "up"},
		{"ip", "route", "add", metadataAddress + "/32", "dev", "md0"},
		{"ip", "-n", metadataNamespace, "addr", "add", metadataAddress + "/32", "dev", "md1"},
		{"ip", "-n", metadataNamespace, "link", "set", "lo", "up"},
		{"ip", "-n", metadataNamespace, "link", "set", "md1", "up"},
		{"ip", "-n", metadataNamespace, "route", "add", metadataPeer + "/32", "dev", "md1"},
		{"ip", "-n", metadataNamespace, "route", "add", "default", "via", metadataPeer},
	}
	for _, step := range steps {
		if out, err := exec.CommandContext(ctx, step[0], step[1:]...).CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(stderr, "stagely-vm-init: %s: %v: %s\n", strings.Join(step, " "), err, strings.TrimSpace(string(out)))
			return 1
		}
	}
	return 0
}

// metadataServeCommand serves the documents in metadata.json on port 80. It
// runs inside the server's network namespace. Without the file it serves
// nothing, as a cloud without metadata for the machine would.
func metadataServeCommand(root string, stderr io.Writer) int {
	docs, err := readMetadata(filepath.Join(root, metadataFile))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "stagely-vm-init:", err)
		return 1
	}
	server := &http.Server{Addr: metadataAddress + ":80", Handler: docs, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		_, _ = fmt.Fprintln(stderr, "stagely-vm-init: serve instance metadata:", err)
		return 1
	}
	return 0
}

func readMetadata(path string) (metadataDocs, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return metadataDocs{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read instance metadata: %w", err)
	}
	var docs metadataDocs
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, fmt.Errorf("instance metadata: %w", err)
	}
	return docs, nil
}

// metadataDocs maps each document's path, such as /metadata/v1/hostname, to
// its content.
type metadataDocs map[string]string

// ServeHTTP answers a document by its path. A path that ends in a slash lists
// the names under it, one per line, with a slash after each directory.
func (d metadataDocs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if body, ok := d[r.URL.Path]; ok {
		contentType := "text/plain; charset=utf-8"
		if strings.HasSuffix(r.URL.Path, ".json") {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
		return
	}
	if names := d.children(r.URL.Path); strings.HasSuffix(r.URL.Path, "/") && len(names) > 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, strings.Join(names, "\n")+"\n")
		return
	}
	http.NotFound(w, r)
}

func (d metadataDocs) children(dir string) []string {
	var names []string
	for path := range d {
		rest, ok := strings.CutPrefix(path, dir)
		if !ok || rest == "" {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if nested {
			name += "/"
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
