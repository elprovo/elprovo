package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Request is one build of a commit on a build worker.
type Request struct {
	Source       Commit
	ComposeFiles []string
	// Config holds plain values that Compose files may interpolate. Secret
	// values never reach build workers.
	Config map[string]string
	// Registry is the image name prefix, such as registry.example.com/acme.
	Registry string
	// Dir is the build's working directory on the worker.
	Dir string
}

// Result is what a build produced.
type Result struct {
	// Project is the resolved Compose project, as `docker compose config`
	// prints it in JSON.
	Project []byte
	// Images maps each built service to its pushed image by digest.
	Images map[string]string
}

var (
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	registryPattern  = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)*$`)
	imagePartPattern = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// Build puts the commit's source on the worker, resolves the Compose
// project as the unprivileged user nobody, checks it against the supported
// subset, builds every service that has a build section, and pushes the
// images to the registry. It removes the source when it is done.
func (s *Source) Build(ctx context.Context, m Machine, req Request) (Result, error) {
	if !registryPattern.MatchString(req.Registry) {
		return Result{}, fmt.Errorf("invalid registry %q", req.Registry)
	}
	src := path.Join(req.Dir, "src")
	defer func() { _, _ = m.Run(context.WithoutCancel(ctx), nil, "sudo", "rm", "-rf", src) }()
	if err := s.Upload(ctx, m, req.Source, src); err != nil {
		return Result{}, err
	}

	compose, err := composeCommand(src, req.ComposeFiles, req.Config)
	if err != nil {
		return Result{}, err
	}
	project, err := m.Run(ctx, nil, slices.Concat([]string{"sudo", "-u", "nobody"}, compose, []string{"config", "--format", "json"})...)
	if err != nil {
		return Result{}, fmt.Errorf("resolve Compose project: %w", err)
	}
	if err := CheckProject(project, src); err != nil {
		return Result{}, err
	}
	refs, err := imageRefs(project, req.Registry, req.Source.Repository, req.Source.SHA)
	if err != nil {
		return Result{}, err
	}
	if len(refs) == 0 {
		return Result{Project: project, Images: map[string]string{}}, nil
	}
	images, err := buildAndPush(ctx, m, slices.Concat([]string{"sudo"}, compose), path.Join(req.Dir, "images.json"), refs)
	if err != nil {
		return Result{}, err
	}
	return Result{Project: project, Images: images}, nil
}

func buildAndPush(ctx context.Context, m Machine, compose []string, overridePath string, refs map[string]string) (map[string]string, error) {
	override, err := json.Marshal(imageOverride(refs))
	if err != nil {
		return nil, fmt.Errorf("encode image names: %w", err)
	}
	if _, err := m.Run(ctx, bytes.NewReader(override), "sudo", "sh", "-c", `cat > "$1"`, "sh", overridePath); err != nil {
		return nil, fmt.Errorf("write image names: %w", err)
	}
	compose = slices.Concat(compose, []string{"-f", overridePath})
	services := slices.Sorted(maps.Keys(refs))
	if err := m.Stream(ctx, nil, slices.Concat(compose, []string{"build", "--pull"}, services)...); err != nil {
		return nil, fmt.Errorf("build images: %w", err)
	}
	if err := m.Stream(ctx, nil, slices.Concat(compose, []string{"push"}, services)...); err != nil {
		return nil, fmt.Errorf("push images: %w", err)
	}
	images := make(map[string]string, len(refs))
	for _, svc := range services {
		digest, err := pushedDigest(ctx, m, refs[svc])
		if err != nil {
			return nil, err
		}
		images[svc] = digest
	}
	return images, nil
}

// composeCommand returns the docker compose command for the source, with the
// config values in its environment for interpolation, for the caller to run
// with sudo. Like a plain `docker compose -f`, it resolves relative paths
// against the directory of the first Compose file, as the control plane's
// planner does.
func composeCommand(src string, files []string, config map[string]string) ([]string, error) {
	args := []string{"env", "BUILDKIT_PROGRESS=plain"}
	for _, k := range slices.Sorted(maps.Keys(config)) {
		if !envNamePattern.MatchString(k) {
			return nil, fmt.Errorf("invalid config name %q", k)
		}
		args = append(args, k+"="+config[k])
	}
	args = append(args, "docker", "compose", "--project-name", "stagelet")
	if len(files) == 0 {
		return nil, errors.New("no Compose files")
	}
	for _, f := range files {
		clean := path.Clean(f)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("compose file %q is outside the source", f)
		}
		args = append(args, "-f", path.Join(src, clean))
	}
	return args, nil
}

// imageRefs names the image of every service with a build section:
// <registry>/<owner>/<repo>/<service>:<commit>.
func imageRefs(project []byte, registry, repository, commit string) (map[string]string, error) {
	var p struct {
		Services map[string]map[string]any `json:"services"`
	}
	if err := json.Unmarshal(project, &p); err != nil {
		return nil, fmt.Errorf("read resolved Compose project: %w", err)
	}
	refs := map[string]string{}
	for name, svc := range p.Services {
		if _, ok := svc["build"]; !ok {
			continue
		}
		parts := strings.Split(strings.ToLower(repository+"/"+name), "/")
		for i, part := range parts {
			parts[i] = imagePartPattern.ReplaceAllString(part, "-")
		}
		refs[name] = registry + "/" + strings.Join(parts, "/") + ":" + commit
	}
	return refs, nil
}

func imageOverride(refs map[string]string) map[string]any {
	services := map[string]any{}
	for svc, ref := range refs {
		services[svc] = map[string]string{"image": ref}
	}
	return map[string]any{"services": services}
}

// pushedDigest returns ref's repository with the digest the registry gave
// the pushed image.
func pushedDigest(ctx context.Context, m Machine, ref string) (string, error) {
	out, err := m.Run(ctx, nil, "sudo", "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", ref)
	if err != nil {
		return "", fmt.Errorf("read digest of %s: %w", ref, err)
	}
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &digests); err != nil {
		return "", fmt.Errorf("read digest of %s: %w", ref, err)
	}
	repo := ref[:strings.LastIndex(ref, ":")]
	for _, d := range digests {
		if strings.HasPrefix(d, repo+"@sha256:") {
			return d, nil
		}
	}
	return "", fmt.Errorf("the registry returned no digest for %s", ref)
}
