// Package runtime runs a stagelet's compiled Compose bundle on its machine:
// it compiles the bundle from a build's resolved project, applies it with
// images pulled by digest, runs the migration command and checks that the
// public service answers.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Machine runs commands on a stagelet machine. Run returns the standard
// output; Stream sends the output to the job log.
type Machine interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	Stream(ctx context.Context, stdin io.Reader, args ...string) error
}

// Paths on the stagelet machine.
const (
	BundlePath  = "/opt/stagelet/compose.json"
	SecretsPath = "/opt/stagelet/secrets.env"
	projectName = "stagelet"
)

var (
	serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// BundleSpec is what a deploy adds to the resolved Compose project.
type BundleSpec struct {
	PublicService string
	PublicPort    int
	// Config holds plain environment values for every service.
	Config map[string]string
	// SecretNames are environment variables every service takes from the
	// secret file on the machine. Their values never enter the bundle.
	SecretNames []string
}

// Compile turns the resolved Compose project of a build into the bundle a
// runtime machine runs: each built service runs its image by digest, every
// service gets the config values and the secret names, and the public
// service publishes its port. The bundle holds no secret values.
func Compile(resolved []byte, images map[string]string, spec BundleSpec) ([]byte, error) {
	if err := checkPublic(spec.PublicService, spec.PublicPort); err != nil {
		return nil, err
	}
	var project map[string]any
	if err := json.Unmarshal(resolved, &project); err != nil {
		return nil, fmt.Errorf("read resolved Compose project: %w", err)
	}
	services, _ := project["services"].(map[string]any)
	if _, ok := services[spec.PublicService]; !ok {
		return nil, fmt.Errorf("public service %q is not in the Compose project", spec.PublicService)
	}
	for name, raw := range services {
		svc, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("service %s is not a mapping", name)
		}
		if err := pinImage(name, svc, images); err != nil {
			return nil, err
		}
		if err := addEnvironment(svc, spec); err != nil {
			return nil, err
		}
		if name == spec.PublicService {
			publishPort(svc, spec.PublicPort)
		}
	}
	delete(project, "name")
	return json.Marshal(project)
}

func pinImage(name string, svc map[string]any, images map[string]string) error {
	if _, built := svc["build"]; !built {
		return nil
	}
	image, ok := images[name]
	if !ok {
		return fmt.Errorf("service %s is built but has no pushed image", name)
	}
	delete(svc, "build")
	if svc["pull_policy"] == "build" {
		delete(svc, "pull_policy")
	}
	svc["image"] = image
	return nil
}

func addEnvironment(svc map[string]any, spec BundleSpec) error {
	env := map[string]any{}
	switch e := svc["environment"].(type) {
	case nil:
	case map[string]any:
		env = e
	default:
		return errors.New("resolved environment must be a mapping")
	}
	for k, v := range spec.Config {
		if !envNamePattern.MatchString(k) {
			return fmt.Errorf("invalid config name %q", k)
		}
		env[k] = v
	}
	for _, k := range spec.SecretNames {
		if !envNamePattern.MatchString(k) {
			return fmt.Errorf("invalid secret name %q", k)
		}
		env[k] = nil
	}
	if len(env) > 0 {
		svc["environment"] = env
	}
	return nil
}

// publishPort makes the machine publish port to the public service's port,
// reusing an entry for that target that publishes no fixed port.
func publishPort(svc map[string]any, port int) {
	want := strconv.Itoa(port)
	ports, _ := svc["ports"].([]any)
	for _, p := range ports {
		m, ok := p.(map[string]any)
		if !ok || fmt.Sprint(m["target"]) != want {
			continue
		}
		if published, set := m["published"]; !set || published == "" {
			m["published"] = want
		}
		if fmt.Sprint(m["published"]) == want {
			return
		}
	}
	svc["ports"] = append(ports, map[string]any{"mode": "ingress", "target": port, "published": want, "protocol": "tcp"})
}

func checkPublic(service string, port int) error {
	if !serviceNamePattern.MatchString(service) {
		return fmt.Errorf("invalid public service %q", service)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid public port %d", port)
	}
	return nil
}

// Apply writes the bundle and the secret values to the machine, then starts
// the project and waits for its health checks. Images are pulled by digest.
// recreate restarts every container even when its config did not change.
func Apply(ctx context.Context, m Machine, bundle []byte, secrets map[string]string, recreate bool) error {
	if err := writeFile(ctx, m, BundlePath, bundle); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	env, err := envFile(secrets)
	if err != nil {
		return err
	}
	if err := writeFile(ctx, m, SecretsPath, env); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	args := compose("up", "--detach", "--remove-orphans", "--wait", "--wait-timeout", "600")
	if recreate {
		args = append(args, "--force-recreate")
	}
	if err := m.Stream(ctx, nil, args...); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	return nil
}

// PinImages returns the bundle with each image that a service names by tag
// replaced by the digest Docker on the machine pulled it at. Call it after
// Apply, so a later reset runs exactly the images this deploy ran.
func PinImages(ctx context.Context, m Machine, bundle []byte) ([]byte, error) {
	var project map[string]any
	if err := json.Unmarshal(bundle, &project); err != nil {
		return nil, fmt.Errorf("read bundle: %w", err)
	}
	services, _ := project["services"].(map[string]any)
	for name, raw := range services {
		svc, _ := raw.(map[string]any)
		image, _ := svc["image"].(string)
		if image == "" || strings.Contains(image, "@") {
			continue
		}
		digest, err := imageDigest(ctx, m, image)
		if err != nil {
			return nil, fmt.Errorf("pin the image of service %s: %w", name, err)
		}
		svc["image"] = digest
	}
	return json.Marshal(project)
}

func imageDigest(ctx context.Context, m Machine, image string) (string, error) {
	out, err := m.Run(ctx, nil, "sudo", "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image)
	if err != nil {
		return "", fmt.Errorf("read digest of %s: %w", image, err)
	}
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &digests); err != nil {
		return "", fmt.Errorf("read digest of %s: %w", image, err)
	}
	want := familiarName(withoutTag(image))
	for _, d := range digests {
		if name, _, _ := strings.Cut(d, "@"); familiarName(name) == want {
			return d, nil
		}
	}
	return "", fmt.Errorf("docker has no digest of %s", image)
}

func withoutTag(image string) string {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i]
	}
	return image
}

// familiarName returns a repository name as Docker shows it, without the
// Docker Hub host and library namespace: postgres for
// docker.io/library/postgres.
func familiarName(repository string) string {
	repository = strings.TrimPrefix(repository, "index.docker.io/")
	repository = strings.TrimPrefix(repository, "docker.io/")
	return strings.TrimPrefix(repository, "library/")
}

// Migrate runs command with sh in the running public service.
func Migrate(ctx context.Context, m Machine, service, command string) error {
	if !serviceNamePattern.MatchString(service) {
		return fmt.Errorf("invalid public service %q", service)
	}
	if err := m.Stream(ctx, nil, compose("exec", "-T", service, "sh", "-c", command)...); err != nil {
		return fmt.Errorf("migration: %w", err)
	}
	return nil
}

// compose returns a docker compose command for the bundle that runs with the
// secret values in its environment, so services that name a secret get it.
func compose(args ...string) []string {
	script := `set -a && . "$1" && set +a && shift && exec docker compose "$@"`
	return append([]string{
		"sudo", "sh", "-c", script, "sh", SecretsPath,
		"--project-name", projectName, "-f", BundlePath,
	}, args...)
}

func writeFile(ctx context.Context, m Machine, path string, data []byte) error {
	_, err := m.Run(ctx, bytes.NewReader(data), "sudo", "sh", "-c", `umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	return err
}

// envFile renders secrets as shell assignments with single-quoted values.
func envFile(secrets map[string]string) ([]byte, error) {
	names := make([]string, 0, len(secrets))
	for k := range secrets {
		if !envNamePattern.MatchString(k) {
			return nil, fmt.Errorf("invalid secret name %q", k)
		}
		names = append(names, k)
	}
	slices.Sort(names)
	var b bytes.Buffer
	for _, k := range names {
		b.WriteString(k + "='" + strings.ReplaceAll(secrets[k], "'", `'\''`) + "'\n")
	}
	return b.Bytes(), nil
}

// WaitHealthy asks the public service for the health path from inside the
// machine until it answers with a 2xx status or the timeout passes. Asking
// from inside needs no network path from the agent to the app's port; the
// gateway checks that path itself.
func WaitHealthy(ctx context.Context, m Machine, port int, healthPath string, timeout time.Duration) error {
	if port < 1 || port > 65535 || !strings.HasPrefix(healthPath, "/") {
		return fmt.Errorf("invalid health check %d %q", port, healthPath)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + healthPath
	probe := `curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" || true`
	last := "no answer"
	for {
		out, err := m.Run(ctx, nil, "sh", "-c", probe, "sh", url)
		code := strings.TrimSpace(string(out))
		switch {
		case err == nil && len(code) == 3 && code[0] == '2':
			return nil
		case err != nil:
			last = err.Error()
		case code != "" && code != "000":
			last = "status " + code
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check %s: %s", url, last)
		case <-time.After(time.Second):
		}
	}
}
