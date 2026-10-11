package build

import (
	"strings"
	"testing"
)

const sourceDir = "/var/lib/stagely/builds/s1/src"

// withService returns a resolved project whose one service has fields.
func withService(fields string) string {
	return `{"services":{"web":{"image":"nginx",` + fields + `}}}`
}

// withBuild returns a resolved project whose one service builds with fields.
func withBuild(fields string) string {
	return withService(`"build":{"context":"` + sourceDir + `",` + fields + `}`)
}

func TestCheckProjectRefusesProjectsOutsideTheSubset(t *testing.T) {
	tests := []struct {
		name    string
		project string
		problem string
	}{
		{"interpolated bind of the root", withService(`"volumes":[{"type":"bind","source":"/","target":"/host"}]`), `volumes: bind mount "/" is not allowed`},
		{"interpolated bind of /var/run", withService(`"volumes":[{"type":"bind","source":"/var/run","target":"/r"}]`), `volumes: bind mount "/var/run" is not allowed`},
		{"bind of a repository directory", withService(`"volumes":[{"type":"bind","source":"` + sourceDir + `/data","target":"/data"}]`), "bind mount"},
		{"named pipe", withService(`"volumes":[{"type":"npipe","source":"x","target":"/x"}]`), `volumes: type "npipe" is not allowed`},
		{"volume with driver options", `{"services":{"web":{"image":"nginx"}},"volumes":{"data":{"driver_opts":{"type":"none","o":"bind","device":"/"}}}}`, "volumes.data: driver_opts are not allowed"},
		{"volume with another driver", `{"services":{"web":{"image":"nginx"}},"volumes":{"data":{"driver":"rexray"}}}`, "volumes.data: driver rexray is not allowed"},
		{"external host network", `{"services":{"web":{"image":"nginx"}},"networks":{"hostnet":{"name":"host","external":true}}}`, "networks.hostnet: external networks are not allowed"},
		{"network named host", `{"services":{"web":{"image":"nginx"}},"networks":{"default":{"name":"host"}}}`, "networks.default: the host network is not allowed"},
		{"macvlan network", `{"services":{"web":{"image":"nginx"}},"networks":{"lan":{"driver":"macvlan"}}}`, "networks.lan: driver macvlan is not allowed"},
		{"network driver options", `{"services":{"web":{"image":"nginx"}},"networks":{"lan":{"driver_opts":{"parent":"eth0"}}}}`, "networks.lan: driver_opts are not allowed"},
		{"secret from a file", `{"services":{"web":{"image":"nginx"}},"secrets":{"tok":{"file":"/root/.docker/config.json"}}}`, "secrets.tok: file sources"},
		{"config from a file", `{"services":{"web":{"image":"nginx"}},"configs":{"cert":{"file":"/etc/ssl/key.pem"}}}`, "configs.cert: file sources"},
		{"privileged", withService(`"privileged":true`), "privileged: privileged containers are not allowed"},
		{"host network mode", withService(`"network_mode":"host"`), "network_mode: host namespaces are not allowed"},
		{"host pid", withService(`"pid":"host"`), "pid: host namespaces are not allowed"},
		{"host ipc", withService(`"ipc":"host"`), "ipc: host namespaces are not allowed"},
		{"host uts", withService(`"uts":"host"`), "uts: host namespaces are not allowed"},
		{"host user namespace", withService(`"userns_mode":"host"`), "userns_mode: host namespaces are not allowed"},
		{"host cgroup namespace", withService(`"cgroup":"host"`), "cgroup: host namespaces are not allowed"},
		{"cgroup parent", withService(`"cgroup_parent":"/system.slice"`), "cgroup_parent: placing the container in a host cgroup"},
		{"added capability", withService(`"cap_add":["SYS_ADMIN"]`), "cap_add: added capabilities are not allowed"},
		{"device", withService(`"devices":[{"source":"/dev/kvm","target":"/dev/kvm"}]`), "devices: devices are not allowed"},
		{"device cgroup rule", withService(`"device_cgroup_rules":["b 8:* rmw"]`), "device_cgroup_rules: devices are not allowed"},
		{"gpus", withService(`"gpus":[{"count":-1}]`), "gpus: devices are not allowed"},
		{"reserved devices", withService(`"deploy":{"resources":{"reservations":{"devices":[{"capabilities":["gpu"]}]}}}`), "deploy.resources.reservations.devices: devices are not allowed"},
		{"other runtime", withService(`"runtime":"runc-unsafe"`), "runtime: other container runtimes are not allowed"},
		{"Docker API socket", withService(`"use_api_socket":true`), "use_api_socket: the Docker socket is not allowed"},
		{"provider service", withService(`"provider":{"type":"./evil"}`), "provider: provider services run programs on the machine"},
		{"label file", withService(`"label_file":["/etc/hosts"]`), "label_file: label files are read from the build worker"},
		{"unconfined seccomp", withService(`"security_opt":["seccomp:unconfined"]`), `security_opt "seccomp:unconfined" weakens isolation`},
		{"unconfined AppArmor", withService(`"security_opt":["apparmor:unconfined"]`), `security_opt "apparmor:unconfined" weakens isolation`},
		{"SELinux labels off", withService(`"security_opt":["label:disable"]`), `security_opt "label:disable" weakens isolation`},
		{"volumes of another container", withService(`"volumes_from":["container:agent"]`), `volumes_from "container:agent": volumes of containers outside the project are not allowed`},
		{"privileged start hook", withService(`"post_start":[{"command":["sh"],"privileged":true}]`), "post_start: privileged hooks are not allowed"},
		{"privileged stop hook", withService(`"pre_stop":[{"command":["sh"],"privileged":true}]`), "pre_stop: privileged hooks are not allowed"},
		{"build context of the worker's root", withService(`"build":{"context":"/"}`), `build.context "/" is not a directory in the repository`},
		{"build context above the source", withService(`"build":{"context":"/var/lib/stagely/builds"}`), `build.context "/var/lib/stagely/builds" is not a directory`},
		{"remote build context", withService(`"build":{"context":"https://github.com/acme/evil.git"}`), `build.context "https://github.com/acme/evil.git" is not a directory`},
		{"absolute Dockerfile", withBuild(`"dockerfile":"/root/.docker/config.json"`), `build.dockerfile "/root/.docker/config.json" is not a file in the repository`},
		{"Dockerfile above the source", withBuild(`"dockerfile":"../../Dockerfile"`), `build.dockerfile "../../Dockerfile" is not a file in the repository`},
		{"additional context of the worker's root", withBuild(`"additional_contexts":{"host":"/"}`), `build.additional_contexts.host "/" is not an image, a service or a directory in the repository`},
		{"remote additional context", withBuild(`"additional_contexts":{"git":"https://github.com/acme/evil.git"}`), "build.additional_contexts.git"},
		{"OCI layout additional context", withBuild(`"additional_contexts":{"oci":"oci-layout:///var/lib/docker"}`), "build.additional_contexts.oci"},
		{"host build network", withBuild(`"network":"host"`), "build.network: host networking is not allowed"},
		{"privileged build", withBuild(`"privileged":true`), "build.privileged: privileged builds are not allowed"},
		{"build entitlements", withBuild(`"entitlements":["security.insecure"]`), "build.entitlements: extra build privileges are not allowed"},
		{"build SSH agent", withBuild(`"ssh":["default"]`), "build.ssh: the build worker's SSH agent and keys are not allowed"},
		{"extra image tags", withBuild(`"tags":["registry.example.com/acme/other:latest"]`), "build.tags: extra tags would push images"},
		{"build cache export", withBuild(`"cache_to":["type=local,dest=/etc"]`), "build.cache_to: exporting the build cache"},
		{"local build cache import", withBuild(`"cache_from":["type=local,src=/root"]`), `build.cache_from "type=local,src=/root": only image names are allowed`},
		{"local cache import after a registry type", withBuild(`"cache_from":["type=registry,type=local,src=/root"]`), "only image names are allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckProject([]byte(tt.project), sourceDir)
			if err == nil || !strings.Contains(err.Error(), "outside the supported subset") || !strings.Contains(err.Error(), tt.problem) {
				t.Fatalf("err = %v, want the subset error mentioning %q", err, tt.problem)
			}
		})
	}
}

func TestCheckProjectAllowsATypicalApp(t *testing.T) {
	project := `{
	  "name": "stagelet",
	  "services": {
	    "web": {
	      "build": {
	        "context": "` + sourceDir + `/app",
	        "dockerfile": "../docker/web.Dockerfile",
	        "additional_contexts": {"base": "docker-image://alpine:3", "shared": "` + sourceDir + `/lib", "assets": "service:assets"},
	        "cache_from": ["registry.example.com/acme/web:cache"],
	        "network": "default",
	        "secrets": [{"source": "npm"}]
	      },
	      "environment": {"MODE": "preview", "DATABASE_URL": "postgres://db/app"},
	      "ports": [{"mode": "ingress", "target": 8080, "protocol": "tcp"}],
	      "volumes": [{"type": "volume", "source": "cache", "target": "/cache"}, {"type": "tmpfs", "target": "/tmp"}],
	      "volumes_from": ["db:ro"],
	      "security_opt": ["no-new-privileges:true"],
	      "networks": {"default": null}
	    },
	    "assets": {"build": {"context": "` + sourceDir + `"}},
	    "db": {"image": "postgres:17", "volumes": [{"type": "volume", "source": "data", "target": "/var/lib/postgresql/data"}]}
	  },
	  "volumes": {"data": {"name": "stagelet_data", "driver": "local"}, "cache": {"name": "stagelet_cache"}},
	  "networks": {"default": {"name": "stagelet_default", "ipam": {}}},
	  "secrets": {"npm": {"name": "stagelet_npm", "environment": "NPM_TOKEN"}}
	}`
	if err := CheckProject([]byte(project), sourceDir); err != nil {
		t.Fatalf("CheckProject: %v", err)
	}
}
