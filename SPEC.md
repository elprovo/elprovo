# Elprovo replay specification, version 1

This document defines the files that Elprovo reads, how `elprovo replay` runs proofs, and what it reports. The [README](README.md) shows the usage.

The words "must" and "must not" state requirements. A file or a command that breaks one is an error.

## 1. Files

A repository that uses Elprovo has two kinds of file:

- `elprovo.yml` at the repository root says how to start, check and stop the app. It is written once and changes rarely.
- Proof files in `proofs/` state one claim each, with the command that shows it.

Both are YAML. Each file must be at most 64 KiB.

### 1.1 `elprovo.yml`

| Field | Required | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `version` | Yes | Integer | | The format version. It must be `1`. |
| `start` | Yes | Command | | Starts the app. It must exit 0. |
| `ready` | Yes | Command | | Exits 0 when the app can take requests. Elprovo runs it once a second until it exits 0. |
| `stop` | No | Command | None | Stops the app and frees what it uses. Elprovo runs it after every run, also after a failure. |
| `env` | No | Map of name to string | Empty | Plain values that every command gets. The file is in the repository, so these must not be secrets. |
| `pass_env` | No | List of names | Empty | Variables that Elprovo copies from its own environment into every command, such as test credentials. A listed variable that is not set is an error. |
| `timeouts.start` | No | Duration | `10m` | The longest time `start` may take. |
| `timeouts.ready` | No | Duration | `5m` | The longest time Elprovo waits for `ready` to exit 0. |
| `timeouts.stop` | No | Duration | `2m` | The longest time `stop` may take. |

### 1.2 Proof files

A proof file is a file named `proofs/<name>.yml`, directly in `proofs/`. `<name>` must match `[a-z0-9][a-z0-9-]*`. Every other file in `proofs/` and in its subdirectories is a helper file that proofs may use. Elprovo does not read helper files.

| Field | Required | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `claim` | Yes | String | | The claim, as one sentence of at most 200 characters, without line breaks. |
| `run` | Yes | Command | | Exits 0 when the claim holds, and with any other status when it does not. |
| `before` | No | `fail` or `skip` | `fail` | What the proof must do on the base commit. `fail`: the proof runs on the base commit and must fail there in every run. `skip`: the proof does not run on the base commit. Use `skip` for a claim that also held before the change, such as a refactor that keeps a behavior. |
| `timeout` | No | Duration | `10m` | The longest time `run` may take. |

### 1.3 Values

- A **command** is a string that Elprovo runs with `/bin/sh -c`. Section 4 defines its directory and environment.
- A **duration** is a whole number followed by `s`, `m` or `h`, such as `90s` or `10m`. It must be at least `1s` and at most `1h`.

### 1.4 Versions

- The `version` in the head commit's `elprovo.yml` sets the format of the proof files too.
- A version 1 reader rejects a field that this document does not define, so that a misspelled field fails instead of being ignored.
- A new field or a change in meaning comes with a new version. A release of Elprovo reads every version it knows, and it rejects a version it does not know.

## 2. Command line

```
elprovo replay [--base REV] [--head REV] [--all] [--runs N] [--require] [--out DIR] [--dry-run] [PROOF...]
```

`elprovo replay` must run inside a Git repository. It reads commits, not the working tree, so it does not replay uncommitted changes.

| Option | Default | Meaning |
| --- | --- | --- |
| `--base REV` | The merge base of the head commit and `origin/HEAD` | The commit without the change. If `--base` is not given and `origin/HEAD` does not exist, Elprovo stops with an error that asks for `--base`. |
| `--head REV` | `HEAD` | The commit with the change. |
| `--all` | Off | Replay every proof file in the head commit, not only the ones that the change adds or modifies. |
| `--runs N` | `2` | The number of runs on each commit. `N` must be from 1 to 10. |
| `--require` | Off | Treat a replay with no selected proof as a failure. |
| `--out DIR` | `.elprovo` | Where Elprovo writes logs, artifacts and the report. |
| `--dry-run` | Off | Check the files and print the selected proofs, without running any command. It exits 0 when the files are valid, 1 when no proof is selected and `--require` is on, and 2 when a file is invalid. |
| `PROOF...` | | Replay only these proof files from the head commit, whether or not the change modifies them. |

Elprovo resolves `--base` and `--head` to commit IDs once, before it runs anything.

## 3. Replay

### 3.1 Selecting proofs

Elprovo selects, in this order of precedence:

1. the proof files named as `PROOF` arguments;
2. with `--all`, every proof file in the head commit;
3. otherwise, every proof file that `git diff --name-only --diff-filter=AM <base> <head>` lists.

A `PROOF` argument that is not a proof file in the head commit is an error. Elprovo replays at most 100 proofs. More is an error.

A change to a helper file alone does not select the proofs that use it. Use `--all` to replay every proof.

### 3.2 Runs

Elprovo replays the selected proofs one at a time, in file name order. For each proof, it alternates the commits: base run 1, head run 1, base run 2, head run 2, and so on. It skips the base runs of a proof whose `before` is `skip`.

Elprovo never runs two runs at the same time, so two apps never compete for the same ports.

One run of proof `P` on commit `C` has these steps:

1. Make a new, empty directory named `elprovo-<run ID>`, where the run ID is 8 random characters from `[a-z0-9]`. Write the tracked files of `C` into it, as `git archive` gives them. The directory has no `.git` directory and no untracked or ignored files.
2. Replace the directory's `proofs/` with `proofs/` from the head commit, so that both commits run the same proof.
3. Take `elprovo.yml` from `C`. If `C` has none, take it from the head commit. If the head commit has none, the replay stops with an error.
4. Make an empty artifacts directory for the run.
5. Run `start`. If it exits with a status other than 0 or takes longer than `timeouts.start`, the run's outcome is **not run**. Go to step 8.
6. Run `ready` once a second until it exits 0. If it does not exit 0 within `timeouts.ready`, the outcome is **not run**. Go to step 8.
7. Run the proof's `run`. If it exits 0, the outcome is **pass**. If it exits with any other status, is ended by a signal, or takes longer than the proof's `timeout`, the outcome is **fail**.
8. If `stop` is set, run it, after every outcome. If `stop` exits with a status other than 0 or takes longer than `timeouts.stop`, the next run could not start clean. Elprovo then stops the whole replay, writes the report for the runs so far, and exits 2.
9. Delete the directory. Keep the logs and the artifacts in the output directory.

Docker Compose names a project after its directory by default, so each run gets its own Compose project without any setting.

### 3.3 Proof results

Elprovo gives each proof one result from its runs. It applies the first rule that matches:

| Rule | Head runs | Base runs | Result |
| --- | --- | --- | --- |
| 1 | At least one is not run | Any | `not-run` |
| 2 | Some pass and some fail | Any | `flaky` |
| 3 | All fail | Any | `failed` |
| 4 | All pass | None, because `before` is `skip` | `verified` |
| 5 | All pass | At least one is not run | `not-run` |
| 6 | All pass | Some pass and some fail | `flaky` |
| 7 | All pass | All pass | `no-effect` |
| 8 | All pass | All fail | `verified` |

### 3.4 Exit status

| Status | When |
| --- | --- |
| 0 | Every selected proof is `verified`. Or no proof is selected, and `--require` is off. |
| 1 | At least one proof is `failed`, `flaky` or `no-effect`. Or no proof is selected, and `--require` is on. |
| 2 | No proof matches status 1, and at least one proof is `not-run`. Or Elprovo could not replay: a file is invalid, a revision does not exist, a variable in `pass_env` is not set, or `stop` failed. |

## 4. How commands run

- Each command runs as `/bin/sh -c '<command>'`, with the run's directory as its working directory, in its own process group.
- When a command takes longer than its timeout, Elprovo sends `SIGTERM` to its process group, then `SIGKILL` 10 seconds later.
- Each command gets only these environment variables:
  - `PATH`, `HOME`, `USER`, `LANG` and `TMPDIR`, from the environment of `elprovo replay`;
  - the variables in `env`;
  - the variables in `pass_env`, from the environment of `elprovo replay`;
  - `ELPROVO_RUN_ID`, the run ID;
  - `ELPROVO_ARTIFACTS`, the absolute path of the run's artifacts directory.
- No variable says which commit runs or whether it is the base or the head commit. A proof that inspects the code to decide its result defeats the replay, and a reviewer should reject it.
- Elprovo keeps the combined standard output and standard error of each command, up to 10 MiB. It drops the rest, and the log says so.
- Elprovo keeps up to 1 GiB of artifacts for each run. It drops the rest, and the report says so.
- Elprovo does not remove secrets from logs. Use test credentials only.

## 5. Output

### 5.1 Directory

Each replay writes to `<out>/runs/<replay ID>/`, where the replay ID is 8 random characters from `[a-z0-9]`:

```
<out>/runs/<replay ID>/
├── report.json
└── <proof name>/
    └── <base|head>-<run number>/
        ├── start.log
        ├── ready.log
        ├── proof.log
        ├── stop.log
        └── artifacts/
```

A log file exists only for a command that ran.

### 5.2 Text

Elprovo prints one block for each proof, then a summary line. The [README](README.md) shows the form. For a `failed` or `flaky` proof, the block also shows the path of `proof.log` from the first head run that failed. For a `not-run` proof, it shows the reason of the first run that did not run. The text form is for people. Programs read `report.json`.

### 5.3 `report.json`

```json
{
  "format": 1,
  "base": "3f2a1c0e9d8b7a6f5e4d3c2b1a0f9e8d7c6b5a49",
  "head": "9b7e4d2a1c0f9e8d7c6b5a493f2a1c0e9d8b7a6f",
  "runs": 2,
  "proofs": [
    {
      "file": "proofs/coupon-once.yml",
      "claim": "A coupon applies once per order, even from two browser tabs.",
      "before": "fail",
      "result": "verified",
      "base": [
        { "run": 1, "outcome": "fail", "exit_code": 1, "timed_out": false, "seconds": 41.2, "reason": "", "dir": "coupon-once/base-1" }
      ],
      "head": [
        { "run": 1, "outcome": "pass", "exit_code": 0, "timed_out": false, "seconds": 39.8, "reason": "", "dir": "coupon-once/head-1" }
      ]
    }
  ]
}
```

The example shows one run of each commit to keep it short.

| Field | Meaning |
| --- | --- |
| `format` | The report format. It is `1`. |
| `base`, `head` | The full commit IDs. |
| `runs` | The number of runs on each commit. |
| `proofs[].result` | `verified`, `failed`, `flaky`, `no-effect` or `not-run`, from section 3.3. |
| `proofs[].base` | The base runs. The list is empty when `before` is `skip`. |
| `outcome` | `pass`, `fail` or `not-run`, from section 3.2. |
| `exit_code` | The exit status of `run`, or `null` when `run` did not exit by itself. |
| `timed_out` | Whether `run` took longer than its timeout. |
| `reason` | Why the outcome is `not-run`, such as `start exited with status 1` or `ready did not exit 0 within 5m`. It is empty otherwise. |
| `dir` | The run's directory, relative to the replay's output directory. |

New fields may appear in a report of the same format. A program that reads reports must ignore fields it does not know.
