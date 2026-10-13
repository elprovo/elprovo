package build

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

var forbiddenServiceKeys = []struct{ key, reason string }{
	{"privileged", "privileged containers are not allowed"},
	{"network_mode", "host namespaces are not allowed"},
	{"pid", "host namespaces are not allowed"},
	{"ipc", "host namespaces are not allowed"},
	{"uts", "host namespaces are not allowed"},
	{"userns_mode", "host namespaces are not allowed"},
	{"cgroup", "host namespaces are not allowed"},
	{"cgroup_parent", "placing the container in a host cgroup is not allowed"},
	{"cap_add", "added capabilities are not allowed"},
	{"devices", "devices are not allowed"},
	{"device_cgroup_rules", "devices are not allowed"},
	{"gpus", "devices are not allowed"},
	{"runtime", "other container runtimes are not allowed"},
	{"use_api_socket", "the Docker socket is not allowed"},
	{"provider", "provider services run programs on the machine and are not allowed"},
	{"label_file", "label files are read from the build worker and are not allowed; use labels"},
}

var forbiddenBuildKeys = []struct{ key, reason string }{
	{"privileged", "privileged builds are not allowed"},
	{"entitlements", "extra build privileges are not allowed"},
	{"ssh", "the build worker's SSH agent and keys are not allowed"},
	{"tags", "extra tags would push images under names that Elprovo does not choose"},
	{"cache_to", "exporting the build cache writes to the build worker or the registry and is not allowed"},
}

var allowedSecurityOpts = []string{"no-new-privileges", "no-new-privileges:true", "no-new-privileges=true"}

type resolvedProject struct {
	Services map[string]map[string]any `json:"services"`
	Volumes  map[string]map[string]any `json:"volumes"`
	Networks map[string]map[string]any `json:"networks"`
	Secrets  map[string]map[string]any `json:"secrets"`
	Configs  map[string]map[string]any `json:"configs"`
}

// CheckProject checks a resolved Compose project, as `docker compose config`
// prints it in JSON, against the supported subset. Every local path the
// project names must be inside sourceDir, the directory it was resolved in.
func CheckProject(project []byte, sourceDir string) error {
	var p resolvedProject
	if err := json.Unmarshal(project, &p); err != nil {
		return fmt.Errorf("read resolved Compose project: %w", err)
	}
	problems := slices.Concat(
		volumeDefinitionProblems(p.Volumes),
		networkDefinitionProblems(p.Networks),
		fileSourceProblems("secrets", p.Secrets),
		fileSourceProblems("configs", p.Configs),
	)
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		for _, problem := range serviceProblems(p.Services[name], sourceDir) {
			problems = append(problems, "service "+name+": "+problem)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the Compose project is outside the supported subset: %s", strings.Join(problems, "; "))
	}
	return nil
}

func volumeDefinitionProblems(volumes map[string]map[string]any) []string {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(volumes)) {
		v := volumes[name]
		if driver, ok := v["driver"]; ok && driver != "local" {
			problems = append(problems, fmt.Sprintf("volumes.%s: driver %v is not allowed; Elprovo uses the local driver", name, driver))
		}
		if _, ok := v["driver_opts"]; ok {
			problems = append(problems, fmt.Sprintf("volumes.%s: driver_opts are not allowed, because they can mount the machine's files", name))
		}
	}
	return problems
}

func networkDefinitionProblems(networks map[string]map[string]any) []string {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(networks)) {
		n := networks[name]
		if driver, ok := n["driver"]; ok && driver != "bridge" {
			problems = append(problems, fmt.Sprintf("networks.%s: driver %v is not allowed; Elprovo uses the bridge driver", name, driver))
		}
		if _, ok := n["driver_opts"]; ok {
			problems = append(problems, fmt.Sprintf("networks.%s: driver_opts are not allowed", name))
		}
		if enabled(n, "external") {
			problems = append(problems, fmt.Sprintf("networks.%s: external networks are not allowed, because they can put services on the machine's network", name))
		}
		if n["name"] == "host" {
			problems = append(problems, fmt.Sprintf("networks.%s: the host network is not allowed", name))
		}
	}
	return problems
}

func fileSourceProblems(kind string, entries map[string]map[string]any) []string {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		if _, ok := entries[name]["file"]; ok {
			problems = append(problems, fmt.Sprintf("%s.%s: file sources read the host's files and are not allowed; use environment or content", kind, name))
		}
	}
	return problems
}

func serviceProblems(svc map[string]any, sourceDir string) []string {
	var problems []string
	for _, f := range forbiddenServiceKeys {
		if enabled(svc, f.key) {
			problems = append(problems, f.key+": "+f.reason)
		}
	}
	if reservesDevices(svc) {
		problems = append(problems, "deploy.resources.reservations.devices: devices are not allowed")
	}
	for _, hook := range []string{"post_start", "pre_stop"} {
		hooks, _ := svc[hook].([]any)
		for _, h := range hooks {
			if m, _ := h.(map[string]any); enabled(m, "privileged") {
				problems = append(problems, hook+": privileged hooks are not allowed")
			}
		}
	}
	problems = append(problems, securityOptProblems(svc["security_opt"])...)
	problems = append(problems, mountProblems(svc)...)
	if b, ok := svc["build"].(map[string]any); ok {
		problems = append(problems, buildProblems(b, sourceDir)...)
	}
	return problems
}

func reservesDevices(svc map[string]any) bool {
	deploy, _ := svc["deploy"].(map[string]any)
	resources, _ := deploy["resources"].(map[string]any)
	reservations, _ := resources["reservations"].(map[string]any)
	_, ok := reservations["devices"]
	return ok
}

func securityOptProblems(v any) []string {
	opts, _ := v.([]any)
	var problems []string
	for _, o := range opts {
		if s, _ := o.(string); !slices.Contains(allowedSecurityOpts, s) {
			problems = append(problems, fmt.Sprintf("security_opt %q weakens isolation and is not allowed; only no-new-privileges is", o))
		}
	}
	return problems
}

func mountProblems(svc map[string]any) []string {
	var problems []string
	volumes, _ := svc["volumes"].([]any)
	for _, v := range volumes {
		m, _ := v.(map[string]any)
		switch typ, _ := m["type"].(string); typ {
		case "volume", "tmpfs":
		case "bind":
			source, _ := m["source"].(string)
			problems = append(problems, fmt.Sprintf("volumes: bind mount %q is not allowed", source))
		default:
			problems = append(problems, fmt.Sprintf("volumes: type %q is not allowed; use a named volume or tmpfs", typ))
		}
	}
	from, _ := svc["volumes_from"].([]any)
	for _, f := range from {
		if s, _ := f.(string); strings.HasPrefix(s, "container:") {
			problems = append(problems, fmt.Sprintf("volumes_from %q: volumes of containers outside the project are not allowed", s))
		}
	}
	return problems
}

func buildProblems(b map[string]any, sourceDir string) []string {
	var problems []string
	for _, f := range forbiddenBuildKeys {
		if enabled(b, f.key) {
			problems = append(problems, "build."+f.key+": "+f.reason)
		}
	}
	buildContext, _ := b["context"].(string)
	if !within(sourceDir, buildContext) {
		problems = append(problems, fmt.Sprintf("build.context %q is not a directory in the repository", buildContext))
	}
	if dockerfile, _ := b["dockerfile"].(string); dockerfile != "" {
		resolved := dockerfile
		if !path.IsAbs(resolved) {
			resolved = path.Join(buildContext, dockerfile)
		}
		if !within(sourceDir, resolved) {
			problems = append(problems, fmt.Sprintf("build.dockerfile %q is not a file in the repository", dockerfile))
		}
	}
	contexts, _ := b["additional_contexts"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(contexts)) {
		c, _ := contexts[name].(string)
		if !strings.HasPrefix(c, "docker-image://") && !strings.HasPrefix(c, "service:") && !within(sourceDir, c) {
			problems = append(problems, fmt.Sprintf("build.additional_contexts.%s %q is not an image, a service or a directory in the repository", name, c))
		}
	}
	if b["network"] == "host" {
		problems = append(problems, "build.network: host networking is not allowed")
	}
	caches, _ := b["cache_from"].([]any)
	for _, c := range caches {
		if s, _ := c.(string); strings.Contains(s, "=") {
			problems = append(problems, fmt.Sprintf("build.cache_from %q: only image names are allowed", s))
		}
	}
	return problems
}

func enabled(m map[string]any, key string) bool {
	v, ok := m[key]
	return ok && v != false
}

func within(dir, p string) bool {
	p = path.Clean(p)
	return path.IsAbs(dir) && path.IsAbs(p) && (p == dir || strings.HasPrefix(p, dir+"/"))
}
