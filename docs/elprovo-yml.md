# .elprovo.yml

`.elprovo.yml` in the root of a repository tells Elprovo how to run a pull request of it as a preview. Elprovo reads it, the Compose files it names, and the files those reference at the pull request's head commit. It does not clone the repository to do so.

```yaml
version: 1
compose:
  - deploy/compose.yml
public:
  service: web
  port: 8080
health:
  path: /healthz
migration:
  command: bin/rails db:migrate
```

| Field | Required | Meaning |
| --- | --- | --- |
| `version` | yes | Always `1` |
| `compose` | yes | Compose files, as paths relative to the repository root. They are combined like `docker compose -f a.yml -f b.yml`. |
| `public.service` | yes | The Compose service that people open |
| `public.port` | yes | The port that service listens on, 1 to 65535 |
| `health.path` | yes | A path on the public service that answers with a 2xx status when the app is ready. It starts with `/`. |
| `migration.command` | no | One line that runs with `sh -c` in the public service after the services start and before the health check. A failed migration fails the deploy. A reset does not run it. |

Unknown fields are an error.

## How a preview runs the project

A build worker builds every service that has a `build` section and pushes its image. The preview's own machine then runs the resolved project with each built service pinned to its image digest. The preview's machine never receives the source, so a service can't read files from the repository at run time.

Relative paths resolve the way `docker compose -f` resolves them: against the directory of the first Compose file in `compose`.

## Config values and secrets

A project in Elprovo has plain config values and secrets. Every service gets each of them as an environment variable, and they win over values in the Compose files.

- Compose files can interpolate plain config values, such as `image: postgres:${POSTGRES_VERSION}`.
- Secrets reach containers only as environment variables. Compose files can't interpolate them, and their values never leave the agent in your cloud.

## The supported Compose subset

The subset is a security control, and Elprovo checks it twice. The control plane checks the Compose files when it plans a deploy. The agent checks the resolved project on the build worker, with every variable filled in, before it builds anything, and compiles the bundle from that checked project. A failed check fails the deploy with an error that names the service and the field.

Elprovo refuses a project that uses any of these, because each one gives a container access to the machine or weakens its isolation:

- `privileged: true`, also on a `post_start` or `pre_stop` hook
- host namespaces: `network_mode`, `pid`, `ipc`, `uts`, `userns_mode`, `cgroup`
- `cap_add`, `cgroup_parent`, `runtime`, and any `security_opt` other than `no-new-privileges`
- devices: `devices`, `device_cgroup_rules`, `gpus` and `deploy.resources.reservations.devices`
- bind mounts, such as `./data:/data`, `/srv:/srv` or `type: bind`, and any mount type other than `volume` and `tmpfs`
- the Docker socket, as a mount or through `use_api_socket`
- `volumes_from` with `container:`. Volumes from another service of the project are fine.
- top-level volumes with `driver_opts` or a driver other than `local`
- top-level networks with `driver_opts`, a driver other than `bridge`, `external: true`, or the name `host`
- top-level `secrets` or `configs` with a `file` source. Use `environment` or `content` sources instead.
- `provider` services, which run a program on the machine
- `label_file`. Use `labels` instead.

Build workers belong to the environment, so the pull requests of all its projects share them, and they hold registry credentials that can push. A `build` section is limited too:

- `context`, `additional_contexts` and `dockerfile` must be paths in the repository. Absolute paths, paths that start with `~`, paths outside the repository and URLs are refused. An additional context can also be an image (`docker-image://...`) or another service (`service:...`).
- `network: host`, `privileged`, `entitlements`, `ssh`, `tags` and `cache_to` are refused.
- `cache_from` takes only image names.

The source itself may not contain a symbolic link that points outside the repository, because Compose and the build would follow it to the build worker's own files.

Named volumes are fine. They live on the preview's machine and survive deploys and resets of the same preview.

### Variables

Compose files can use variables from the plain config values and from the repository's `.env` file in most fields, such as `image`, `environment` and `ports`. The build worker fills them in after the plan, so the control plane can't know their values. It refuses a variable in every field that the subset checks:

- `volumes` entries in short syntax, and the `type` and `source` of a long-syntax volume
- `volumes_from` and `security_opt`
- the build's `context`, `additional_contexts`, `dockerfile`, `network`, `secrets` and `cache_from`
- the `driver` of a top-level volume, and the `driver` and `name` of a top-level network
- paths in `include`, `extends` and `env_file`

## include, extends and env_file

Elprovo follows these references and checks every Compose file it reaches against the same subset:

| Reference | Resolved against |
| --- | --- |
| `include` (short or long form, with `project_directory` and `env_file`) | The project directory of the file that names it. An included file's own references resolve against its own directory, or its `project_directory`. |
| `extends.file` | The directory of the file that names it. Without `file`, the service must be in the same file. |
| `env_file` (string, list, or entries with `path` and `required`) | The project directory of the file that names it. An entry with `required: false` may be missing. |

A plan fails, with an error that names the reference, when a referenced file does not exist at the commit, when a path points outside the repository, when a path is a URL, such as a remote `include`, or when a path uses a variable such as `${STAGE}.env`. A plan reads at most 32 files, and references nest at most 4 levels deep.
