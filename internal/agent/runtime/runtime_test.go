package runtime

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

const resolved = `{
  "name": "preview",
  "services": {
    "db": {"image": "postgres:17", "environment": {"POSTGRES_PASSWORD": "dev"}},
    "web": {
      "build": {"context": "/var/lib/elprovo/builds/s1/src", "dockerfile": "Dockerfile"},
      "pull_policy": "build",
      "environment": {"LITERAL": "cost $$5", "MODE": "dev"},
      "ports": [{"mode": "ingress", "target": 8080, "protocol": "tcp"}]
    }
  }
}`

func compileBundle(t *testing.T) map[string]any {
	t.Helper()
	bundle, err := Compile([]byte(resolved), map[string]string{"web": "registry.example.com/acme/web/web@sha256:aa"}, BundleSpec{
		PublicService: "web", PublicPort: 8080, Config: map[string]string{"MODE": "preview"}, SecretNames: []string{"API_TOKEN"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(bundle, &out); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	return out
}

func TestCompiledBundleRunsBuiltServicesByDigest(t *testing.T) {
	web := compileBundle(t)["services"].(map[string]any)["web"].(map[string]any)

	if web["image"] != "registry.example.com/acme/web/web@sha256:aa" || web["build"] != nil || web["pull_policy"] != nil {
		t.Fatalf("web = %v, want the pushed image and no build", web)
	}
	ports := web["ports"].([]any)
	if len(ports) != 1 || ports[0].(map[string]any)["published"] != "8080" {
		t.Fatalf("ports = %v, want 8080 published once", ports)
	}
}

func TestCompiledBundleGivesEveryServiceConfigAndSecretNames(t *testing.T) {
	services := compileBundle(t)["services"].(map[string]any)

	web := services["web"].(map[string]any)["environment"].(map[string]any)
	wantWeb := map[string]any{"LITERAL": "cost $$5", "MODE": "preview", "API_TOKEN": nil}
	if !reflect.DeepEqual(web, wantWeb) {
		t.Fatalf("web environment = %v, want %v", web, wantWeb)
	}
	db := services["db"].(map[string]any)["environment"].(map[string]any)
	if db["MODE"] != "preview" || db["API_TOKEN"] != nil || db["POSTGRES_PASSWORD"] != "dev" {
		t.Fatalf("db environment = %v, want its own values plus config and the secret name", db)
	}
}

func TestCompileRejectsABuiltServiceWithoutAnImage(t *testing.T) {
	_, err := Compile([]byte(resolved), nil, BundleSpec{PublicService: "web", PublicPort: 8080})
	if err == nil || !strings.Contains(err.Error(), "service web is built") {
		t.Fatalf("err = %v, want a missing image error", err)
	}
}

type call struct {
	args  []string
	stdin string
}

type recordingMachine struct{ calls []call }

func (m *recordingMachine) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	m.record(stdin, args)
	return nil, nil
}

func (m *recordingMachine) Stream(_ context.Context, stdin io.Reader, args ...string) error {
	m.record(stdin, args)
	return nil
}

func (m *recordingMachine) record(stdin io.Reader, args []string) {
	c := call{args: args}
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		c.stdin = string(data)
	}
	m.calls = append(m.calls, c)
}

func TestApplyWritesBundleAndSecretsThenStartsTheProject(t *testing.T) {
	m := &recordingMachine{}

	if err := Apply(context.Background(), m, []byte(`{"services":{}}`), map[string]string{"API_TOKEN": "it's secret"}, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(m.calls) != 3 {
		t.Fatalf("calls = %v, want bundle, secrets and up", m.calls)
	}
	if m.calls[0].args[len(m.calls[0].args)-1] != BundlePath || m.calls[0].stdin != `{"services":{}}` {
		t.Fatalf("first call = %v, want the bundle written to %s", m.calls[0], BundlePath)
	}
	if m.calls[1].args[len(m.calls[1].args)-1] != SecretsPath || m.calls[1].stdin != "API_TOKEN='it'\\''s secret'\n" {
		t.Fatalf("second call = %v, want quoted secrets written to %s", m.calls[1], SecretsPath)
	}
	if !strings.Contains(strings.Join(m.calls[1].args, " "), "umask 077") {
		t.Fatalf("second call = %v, want the secrets file created readable by root only", m.calls[1])
	}
	up := strings.Join(m.calls[2].args, " ")
	if !strings.Contains(up, SecretsPath+" --project-name preview -f "+BundlePath+" up") || !strings.HasSuffix(up, "--force-recreate") {
		t.Fatalf("up = %s, want compose up of the bundle with the secrets loaded", up)
	}
}

type inspectingMachine struct {
	recordingMachine
	repoDigests map[string]string
}

func (m *inspectingMachine) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	_, _ = m.recordingMachine.Run(ctx, stdin, args...)
	return []byte(m.repoDigests[args[len(args)-1]]), nil
}

func TestPinnedBundleRunsEveryImageByDigest(t *testing.T) {
	bundle := `{"services":{
		"db": {"image": "postgres:17"},
		"cache": {"image": "docker.io/library/redis:7"},
		"web": {"image": "registry.example.com/acme/web/web@sha256:aa"}}}`
	m := &inspectingMachine{repoDigests: map[string]string{
		"postgres:17":               `["postgres@sha256:bb"]`,
		"docker.io/library/redis:7": `["mirror.example.com/redis@sha256:ff","redis@sha256:cc"]`,
	}}

	pinned, err := PinImages(context.Background(), m, []byte(bundle))
	if err != nil {
		t.Fatalf("PinImages: %v", err)
	}

	var out struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal(pinned, &out); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	got := map[string]string{}
	for name, svc := range out.Services {
		got[name] = svc.Image
	}
	want := map[string]string{"db": "postgres@sha256:bb", "cache": "redis@sha256:cc", "web": "registry.example.com/acme/web/web@sha256:aa"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("images = %v, want %v", got, want)
	}
	if len(m.calls) != 2 {
		t.Fatalf("calls = %v, want one inspect for each image named by tag", m.calls)
	}
}

func TestPinImagesFailsWhenDockerHasNoDigest(t *testing.T) {
	m := &inspectingMachine{repoDigests: map[string]string{"postgres:17": `[]`}}

	_, err := PinImages(context.Background(), m, []byte(`{"services":{"db":{"image":"postgres:17"}}}`))

	if err == nil || !strings.Contains(err.Error(), "service db") {
		t.Fatalf("err = %v, want an error naming service db", err)
	}
}

func TestMigrateRunsTheCommandInThePublicService(t *testing.T) {
	m := &recordingMachine{}
	if err := Migrate(context.Background(), m, "web", "bin/rails db:migrate"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := m.calls[0].args
	if want := []string{"exec", "-T", "web", "sh", "-c", "bin/rails db:migrate"}; !reflect.DeepEqual(got[len(got)-6:], want) {
		t.Fatalf("args = %v, want to end with %v", got, want)
	}
}

// probingMachine answers health probes with codes, one per call.
type probingMachine struct {
	recordingMachine
	codes []string
}

func (m *probingMachine) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	_, _ = m.recordingMachine.Run(ctx, stdin, args...)
	code := m.codes[0]
	if len(m.codes) > 1 {
		m.codes = m.codes[1:]
	}
	return []byte(code), nil
}

func TestWaitHealthyRetriesUntilServiceAnswers(t *testing.T) {
	m := &probingMachine{codes: []string{"000", "503", "200"}}

	if err := WaitHealthy(context.Background(), m, 8080, "/healthz", 10*time.Second); err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
	if len(m.calls) != 3 || m.calls[0].args[len(m.calls[0].args)-1] != "http://127.0.0.1:8080/healthz" {
		t.Fatalf("calls = %v, want three probes of the local health URL", m.calls)
	}
}

func TestWaitHealthyFailsAfterTimeoutWithTheLastStatus(t *testing.T) {
	m := &probingMachine{codes: []string{"500"}}

	err := WaitHealthy(context.Background(), m, 8080, "/healthz", 1500*time.Millisecond)

	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v, want a timeout naming status 500", err)
	}
}
