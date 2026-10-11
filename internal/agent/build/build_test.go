package build

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

type recordingMachine struct {
	copied  []byte
	runs    [][]string
	stdins  map[string]string
	outputs map[string]string
}

func (m *recordingMachine) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	m.runs = append(m.runs, args)
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		if m.stdins == nil {
			m.stdins = map[string]string{}
		}
		m.stdins[args[len(args)-1]] = string(data)
	}
	for suffix, out := range m.outputs {
		if strings.HasSuffix(strings.Join(args, " "), suffix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func (m *recordingMachine) Stream(ctx context.Context, stdin io.Reader, args ...string) error {
	_, err := m.Run(ctx, stdin, args...)
	return err
}

func (m *recordingMachine) CopyIn(_ context.Context, localPath, _ string) error {
	data, err := os.ReadFile(localPath)
	m.copied = data
	return err
}

func TestUploadCopiesTarballAndUnpacksIt(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/web/tarball/"+commit || r.Header.Get("Authorization") != "Bearer tok" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "tarball-bytes")
	}))
	defer api.Close()
	m := &recordingMachine{}
	src := &Source{HTTP: api.Client(), MaxBytes: 1 << 20}
	agentTemp := t.TempDir()
	t.Setenv("TMPDIR", agentTemp)

	if err := src.Upload(context.Background(), m, Commit{APIURL: api.URL, Repository: "acme/web", SHA: commit, Token: "tok"}, "/opt/stagelet/src"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if string(m.copied) != "tarball-bytes" {
		t.Fatalf("copied %q, want the downloaded tarball", m.copied)
	}
	if left, _ := os.ReadDir(agentTemp); len(left) != 0 {
		t.Fatalf("the agent host kept %v after the upload, want its copy of the source removed", left)
	}
	want := [][]string{
		{"sudo", "rm", "-rf", "/opt/stagelet/src"},
		{"sudo", "mkdir", "-p", "/opt/stagelet/src"},
		{"sudo", "tar", "-xzf", "/tmp/stagely-source.tar.gz", "-C", "/opt/stagelet/src", "--strip-components=1"},
		{"rm", "-f", "/tmp/stagely-source.tar.gz"},
		{"sudo", "find", "/opt/stagelet/src", "-type", "l", "-exec", "realpath", "-m", "-z", "--", "{}", "+"},
	}
	if !reflect.DeepEqual(m.runs, want) {
		t.Fatalf("commands = %v, want %v", m.runs, want)
	}
}

func TestUploadRefusesASymbolicLinkThatLeavesTheSource(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tarball-bytes")
	}))
	defer api.Close()
	tests := []struct {
		name    string
		targets string
		problem string
	}{
		{"inside the source", "/opt/stagelet/src/.env.example\x00/opt/stagelet/src\x00", ""},
		{"a worker file", "/opt/stagelet/src/README\x00/root/.docker/config.json\x00", "symbolic link to /root/.docker/config.json"},
		{"above the source", "/opt/stagelet\x00", "symbolic link to /opt/stagelet,"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &recordingMachine{outputs: map[string]string{"{} +": tt.targets}}
			src := &Source{HTTP: api.Client(), MaxBytes: 1 << 20}

			err := src.Upload(context.Background(), m, Commit{APIURL: api.URL, Repository: "acme/web", SHA: commit}, "/opt/stagelet/src")

			if tt.problem == "" && err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if tt.problem != "" && (err == nil || !strings.Contains(err.Error(), tt.problem)) {
				t.Fatalf("Upload error = %v, want one mentioning %q", err, tt.problem)
			}
		})
	}
}

func TestUploadRejectsTarballOverLimit(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 11))
	}))
	defer api.Close()
	src := &Source{HTTP: api.Client(), MaxBytes: 10}

	err := src.Upload(context.Background(), &recordingMachine{}, Commit{APIURL: api.URL, Repository: "acme/web", SHA: commit}, "/opt/stagelet/src")
	if err == nil || !strings.Contains(err.Error(), "larger than 10 bytes") {
		t.Fatalf("Upload error = %v, want a size-limit error", err)
	}
}

func TestUploadRejectsUnsafeRepositoryName(t *testing.T) {
	src := &Source{HTTP: http.DefaultClient, MaxBytes: 10}
	if err := src.Upload(context.Background(), &recordingMachine{}, Commit{APIURL: "http://unused", Repository: "acme/../evil", SHA: commit}, "/opt/stagelet/src"); err == nil {
		t.Fatal("Upload accepted a repository name with path segments")
	}
}

func TestBuildPushesBuiltServicesAndReturnsTheirDigests(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tarball-bytes")
	}))
	defer api.Close()
	ref := "registry.example.com/acme/acme/web/api"
	m := &recordingMachine{outputs: map[string]string{
		"config --format json": `{"services":{"api":{"build":{"context":"/var/lib/stagely/builds/s1/src"}},"db":{"image":"postgres:17"}}}`,
		ref + ":" + commit:     `["` + ref + `@sha256:abc"]` + "\n",
	}}
	src := &Source{HTTP: api.Client(), MaxBytes: 1 << 20}

	res, err := src.Build(context.Background(), m, Request{
		Source: Commit{APIURL: api.URL, Repository: "Acme/Web", SHA: commit}, ComposeFiles: []string{"compose.yml"},
		Config: map[string]string{"MODE": "preview"}, Registry: "registry.example.com/acme", Dir: "/var/lib/stagely/builds/s1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if !reflect.DeepEqual(res.Images, map[string]string{"api": ref + "@sha256:abc"}) {
		t.Fatalf("images = %v, want the api image by digest", res.Images)
	}
	if !strings.Contains(m.stdins["/var/lib/stagely/builds/s1/images.json"], `"image":"`+ref+":"+commit+`"`) {
		t.Fatalf("image names = %v, want the api image tagged with the commit", m.stdins)
	}
	commands := make([]string, len(m.runs))
	for i, r := range m.runs {
		commands[i] = strings.Join(r, " ")
	}
	all := strings.Join(commands, "\n")
	for _, want := range []string{
		"sudo -u nobody env BUILDKIT_PROGRESS=plain MODE=preview docker compose --project-name stagelet -f /var/lib/stagely/builds/s1/src/compose.yml config --format json",
		"sudo env BUILDKIT_PROGRESS=plain MODE=preview docker compose", "build --pull api", "push api", "sudo rm -rf /var/lib/stagely/builds/s1/src",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("commands =\n%s\nwant one with %q", all, want)
		}
	}
}

func TestBuildRefusesAProjectOutsideTheSubsetBeforeBuilding(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tarball-bytes")
	}))
	defer api.Close()
	m := &recordingMachine{outputs: map[string]string{
		"config --format json": `{"services":{"api":{"build":{"context":"/"}}}}`,
	}}
	src := &Source{HTTP: api.Client(), MaxBytes: 1 << 20}

	_, err := src.Build(context.Background(), m, Request{
		Source: Commit{APIURL: api.URL, Repository: "acme/web", SHA: commit}, ComposeFiles: []string{"compose.yml"},
		Registry: "registry.example.com/acme", Dir: "/var/lib/stagely/builds/s1",
	})

	if err == nil || !strings.Contains(err.Error(), `service api: build.context "/" is not a directory in the repository`) {
		t.Fatalf("Build error = %v, want the build context refused", err)
	}
	last := strings.Join(m.runs[len(m.runs)-1], " ")
	for _, r := range m.runs {
		if command := strings.Join(r, " "); strings.Contains(command, " build ") || strings.Contains(command, " push ") {
			t.Fatalf("ran %q, want nothing built or pushed", command)
		}
	}
	if last != "sudo rm -rf /var/lib/stagely/builds/s1/src" {
		t.Fatalf("last command = %q, want the source removed", last)
	}
}

func TestBuildRejectsAnInvalidRegistry(t *testing.T) {
	src := &Source{HTTP: http.DefaultClient, MaxBytes: 10}
	_, err := src.Build(context.Background(), &recordingMachine{}, Request{Registry: "https://registry.example.com"})
	if err == nil || !strings.Contains(err.Error(), "invalid registry") {
		t.Fatalf("err = %v, want an invalid registry error", err)
	}
}

func TestUploadRejectsAnInvalidAPIURL(t *testing.T) {
	src := &Source{HTTP: http.DefaultClient, MaxBytes: 10}
	err := src.Upload(context.Background(), &recordingMachine{}, Commit{APIURL: "file:///etc", Repository: "acme/web", SHA: commit}, "/opt/stagelet/src")
	if err == nil || !strings.Contains(err.Error(), "invalid GitHub API URL") {
		t.Fatalf("err = %v, want an invalid API URL error", err)
	}
}
