# The agent

`elprovo-agent` runs in your cloud next to your credentials and secrets. It opens one outbound WebSocket session to the Elprovo control plane, takes the jobs the control plane leases to it, and drives machines over SSH. It accepts no inbound connections, and no Elprovo code runs on the machines it drives.

## What a deploy does

The control plane sends a deploy job with the repository, the commit, the Compose files, the public service, the health path, an optional migration command, plain config values and the names of secrets. The agent then runs these steps and reports each one, with its output, while it works:

| Step | Where | What happens |
| --- | --- | --- |
| `secrets` | Agent host | Finds the value of each named secret in the agent's store or secrets file. A missing secret fails the deploy here, with an error that names the key. Skipped when the deploy names no secrets. |
| `provision` | Cloud | Creates the preview's machine, `<prefix><preview id>`, unless it exists. Runs at the same time as `build`. |
| `build` | Build worker | Takes a build worker from the pool, copies the commit's source to it, resolves the Compose project with `docker compose config` as the unprivileged user `nobody`, so Compose can't read files that only root can read, such as the registry credentials, and checks the resolved project against the supported Compose subset. Then it builds each service that has a `build` section and pushes its image to the registry. The agent records each image by digest. A source with a symbolic link that points outside it, or a project outside the subset, fails the step before anything is built. The source is removed from the worker after the build. |
| `start` | Preview machine | Writes the compiled bundle and the secret file, signs Docker in to the registry with credentials that can only pull where the cloud allows it, and runs `docker compose up --wait`. Built images are pulled by digest. Then the agent reads the digest of each image that a service names by tag, as the machine pulled it. |
| `migrate` | Preview machine | Runs the migration command with `sh -c` in the running public service. Skipped when `.elprovo.yml` names no migration command. A failed migration fails the deploy. |
| `health` | Preview machine | Requests the health path with `curl` from inside the machine until it answers with a 2xx status, for up to two minutes. The agent host needs no network path to the app's port; the gateway checks that path itself and reports it as route health. |

The result goes back under the job's lease, with the machine's address, the image digests and the compiled bundle. The control plane keeps the bundle, so a later reset can apply exactly what this deploy applied.

Every deploy or reset result also says whether the job started to apply its bundle on the preview machine (`machine_changed`). That happens in the `start` step, before `docker compose up`. A job that fails earlier, such as in `secrets`, `build` or `provision`, leaves the machine running the app it ran before. A job that fails later may leave it running parts of the new bundle.

### The compiled bundle

The bundle is the resolved Compose project, changed so that the preview machine can run it without the source:

- Each built service runs its pushed image by digest, and loses its `build` section.
- After the `start` step, each other service's image is pinned to the digest the machine pulled. The bundle the agent returns names every image by digest, so a reset runs the same images even when a tag has moved.
- Every service gets the plain config values as environment variables. They win over the values in the Compose files.
- Every service gets each secret name as an environment variable without a value. Compose takes the value from `/opt/preview/secrets.env`, which only root can read. Secret values never enter the bundle.
- The public service publishes its port on the machine, so the gateway can reach it.

The agent compiles the bundle from the resolved project that it checked against the supported Compose subset before the build.

Compose files can interpolate plain config values, such as `${MODE}`, because the build resolves the project with them. Secrets reach containers only as environment variables; Compose files cannot interpolate them. [elprovo-yml.md](elprovo-yml.md) describes the repository side: `.elprovo.yml` and the supported Compose subset.

## Reset

A reset job carries the bundle of the preview's last applied deploy. The agent writes it and the current secret values to the machine, recreates every container and checks health. It fetches no source, builds nothing and runs no migration, so it does not undo data migrations.

## Build workers

Build workers belong to the environment, not to a preview. The pool has a fixed size, set with `-build-workers`; workers are named `<prefix>build-1` to `<prefix>build-<size>`. A worker is created the first time a build needs it and kept for later builds, so its Docker layer cache helps the next build. After each build, also a failed one, the agent signs the worker out of the registry and deletes its BuildKit cache mounts (`RUN --mount=type=cache`), because a pull request's Dockerfile can write anything to them and the next build may be of another pull request or project. The layer cache stays, because each layer is keyed by its inputs. If this cleanup fails, the agent deletes the worker, and the next build creates it again. After a successful build the agent also removes images older than 72 hours from the worker. When the pool shrinks, the agent deletes workers above the new size.

## Logs and secrets

Every line a command prints goes to the control plane, at most once a second in batches, tagged with its step and stream. The control plane keeps the newest 5000 lines of each operation. Before a line leaves the agent, the agent replaces every secret value and every registry password it signed a machine in with by `[secret]`. Step messages and job errors pass through the same filter.

## Secrets

Secret values stay on the agent host. The control plane stores only the key of each secret and the environment variable name it fills.

```sh
printf %s "$DATABASE_PASSWORD" | elprovo-agent secrets set db_password
elprovo-agent secrets list
elprovo-agent secrets delete db_password
```

The store is `<data-dir>/secrets.json`, sealed with AES-256-GCM under the key in `<data-dir>/secrets.key`. The agent creates the key the first time you set a secret. Keep both files when you move the agent; without the key file the store cannot be read. The encryption protects a copy of the store file that leaks without the key file, such as a backup of one file; it does not protect against someone who can read the whole data directory.

A pipeline can supply values in a dotenv file instead, with `-secrets-file`. Lines look like `KEY=value`, optionally with `export` and quotes. When the store and the file hold the same key, the file wins. Keys are 1 to 128 letters, digits, dots, dashes or underscores.

## Jobs across reconnects and restarts

While a job runs, the agent renews its lease every 20 seconds. When the control plane rejects a renewal, because the lease expired or the job is no longer wanted, it abandons the job, and the agent stops it.

A job keeps running when the session drops. After the agent reconnects, it lists the jobs it still holds, with their lease tokens. The control plane adopts a job whose lease is still live and abandons the rest. The agent stops an abandoned job and drops its result, so stale work cannot change a preview. A newer job for the same preview or machine waits, still renewing its lease, until the agent's earlier job there has returned and cleaned up, so two jobs never work on one machine at once. A finished job whose result the agent could not send is sent again once the control plane adopts it.

The agent keeps a journal in `<data-dir>/journal.db`, a SQLite file that only its user can read. Before each side effect it records the intent, and after it clears or completes the record:

- Before it takes a job, it records the job. If the journal cannot record it, the agent refuses the job.
- When a job ends, it records the result until the control plane has it.
- Before it creates or deletes a machine, it records that, and clears the record when the provider confirms.

After a restart, the agent lists the journaled jobs for adoption. An adopted job that had not finished runs again from the start; each step is safe to repeat. A machine whose create or delete did not finish is deleted, because its state is unknown; the next deploy creates it again. Jobs, including adopted ones, start only after those deletes finish. A create that fails deletes whatever the provider made. So a restart during a deploy or a delete leaves no machine that Elprovo does not know about.

## Inventory

After it connects, and every five minutes, the agent lists the machines under its tag whose names it creates: build workers, `<prefix>build-<n>`, and preview machines, `<prefix><preview id>`, where a preview id is 12 lowercase hex characters. Machines with other names are never reported and never deleted, even when their names start with the prefix, such as the local environment's gateway VM. The control plane deletes preview machines that no live preview owns by sending a `delete_machine` job; the agent refuses that job for any name it does not create.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-control-plane` | `wss://app.elprovo.com/v1/connect` | Control plane session URL: the agent hub of Elprovo's hosted control plane by default. `elprovo setup` sets it to the control plane you signed in to. |
| `-data-dir` | `~/.elprovo/agent` | Agent key, SSH key, host keys and secret store |
| `-enrollment-token` | none | One-time token, needed on the first start only |
| `-provider` | `lima` | `lima` for local testing, `digitalocean`, `aws`, `gcp`, `hetzner`, `vultr` or `azure` |
| `-registry` | none, required | Image name prefix, such as `registry.digitalocean.com/acme` |
| `-registry-username` | none | Registry user name. The password comes from `ELPROVO_REGISTRY_PASSWORD`. Without it, a provider whose cloud has a registry, such as DigitalOcean, AWS or Google Cloud, hands out short-lived credentials for each job. |
| `-build-workers` | `2` | Size of the build-worker pool |
| `-max-jobs` | `2` | Jobs that run at the same time |
| `-secrets-file` | none | Dotenv file with secret values from your pipeline |
| `-machine-prefix` | `elprovo-` | Prefix of every machine name the agent creates |

The DigitalOcean flags are in [digitalocean.md](digitalocean.md), the AWS flags in [aws.md](aws.md), the Hetzner Cloud flags in [hetzner.md](hetzner.md), the Vultr flags in [vultr.md](vultr.md), the Azure flags in [azure.md](azure.md), and the Lima flags in [local-environment.md](local-environment.md).
