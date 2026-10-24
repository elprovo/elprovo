# Elprovo replay specification, version 1

This document defines the files that Elprovo reads, how `elprovo replay` runs proofs, and what it reports. The [README](README.md) shows the usage.

The words "must" and "must not" state requirements. A file or a command that breaks one is an error.

Elprovo runs on macOS and Linux. It needs `git` and `/bin/sh`. It does not support Windows: there, `elprovo replay` stops before it runs a command.

## 1. Files

A repository that uses Elprovo has two kinds of file:

- `elprovo.yml` at the repository root says how to start, check and stop the app. It is written once and changes rarely.
- Proof files in `proofs/` state one claim each, with the command that shows it.

Each file must be one YAML document that holds a mapping, and must be at most 64 KiB.

### 1.1 `elprovo.yml`

| Field | Required | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `version` | Yes | Integer | | The format version. It must be `1`. |
| `start` | Yes | Command | | Starts the app, or builds it when there is nothing to keep running. It must exit 0. |
| `ready` | Yes | Command | | Exits 0 when the app can take requests. Elprovo runs it once a second until it exits 0. |
| `stop` | No | Command | None | Stops the app and frees what it uses, such as containers, volumes and processes. Elprovo runs it after every run, also after a failure. Leave it out when `start` leaves nothing running. |
| `env` | No | Map of name to string | Empty | Plain values that every command gets. The file is in the repository, so these must not be secrets. |
| `pass_env` | No | List of names | Empty | Variables that Elprovo copies from its own environment into every command, such as test credentials or `DOCKER_HOST`. A listed variable that is not set is an error. |
| `timeouts.start` | No | Duration | `10m` | The longest time `start` may take. |
| `timeouts.ready` | No | Duration | `5m` | The longest time Elprovo waits for `ready` to exit 0. |
| `timeouts.stop` | No | Duration | `2m` | The longest time `stop` may take. |

A name in `env` or `pass_env` must match `[A-Za-z_][A-Za-z0-9_]*` and must not start with `ELPROVO_`. A value in `env` is text: Elprovo takes a YAML scalar as written, so `8080` and `"8080"` give the same value. A missing value, `~` or `null` gives an empty value.

`pass_env` may name `HOME` or `TMPDIR`. The command then gets the real directory instead of the run's own empty one, as section 4 says. Do this only when a tool cannot work without it, because commands can then change files there.

### 1.2 Proof files

A proof file is a file named `proofs/<name>.yml`, directly in `proofs/`. `<name>` must match `[a-z0-9][a-z0-9-]*`. Every other file in `proofs/` and in its subdirectories is a helper file that proofs may use. Elprovo does not read helper files, and a helper file is never a proof.

| Field | Required | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `claim` | Yes | String | | The claim, as one sentence of at most 200 characters, without line breaks. |
| `run` | Yes | Command | | Exits 0 when the claim holds, and with any other status when it does not. |
| `before` | No | `fail` or `skip` | `fail` | What the proof must do on the base commit. `fail`: the proof must fail there in every run. `skip`: the proof does not run on the base commit. Use `skip` for a claim that also held before the change, such as a refactor that keeps a behavior. |
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
elprovo version
```

`elprovo replay` must run inside a Git repository. It reads commits, not the working tree, so it does not replay uncommitted changes. Options come before `PROOF` arguments.

| Option | Default | Meaning |
| --- | --- | --- |
| `--base REV` | The merge base of the head commit and `origin/HEAD` | The commit without the change. If `--base` is not given and `origin/HEAD` does not exist, Elprovo stops with an error that asks for `--base`. |
| `--head REV` | `HEAD` | The commit with the change. |
| `--all` | Off | Also replay the proofs that the change does not add or modify, on the head commit only. |
| `--runs N` | `2` | The number of runs on each commit. `N` must be from 1 to 10. |
| `--require` | Off | Treat a replay with no selected proof as a failure. |
| `--out DIR` | `.elprovo` | Where Elprovo writes logs, artifacts and the report. A relative path is relative to the working directory. |
| `--dry-run` | Off | Check the files and print the selected proofs, without running any command. It exits 0 when the files are valid, 1 when no proof is selected and `--require` is on, and 2 when a file is invalid. |
| `PROOF...` | | Replay only these proof files from the head commit. A relative path is relative to the working directory. |

Elprovo resolves `--base` and `--head` to commit IDs once, before it runs anything.

## 3. Replay

### 3.1 Selecting proofs

A proof is **claimed** when the change adds or modifies its file, as `git diff --name-only --no-renames --diff-filter=AM <base> <head>` lists it. A renamed proof file counts as added. Only a claimed proof says something about the change, so only a claimed proof runs on the base commit.

Elprovo selects:

1. with `PROOF` arguments, those proof files;
2. otherwise, with `--all`, every proof file in the head commit;
3. otherwise, every claimed proof file.

A selected proof that is not claimed runs on the head commit only. It shows that its claim still holds.

A `PROOF` argument that is not a proof file in the head commit is an error. Elprovo replays at most 100 proofs. More is an error.

A change to a helper file alone does not claim the proofs that use it. Use `--all` to replay them.

Before it runs any command, Elprovo checks every selected proof file, the head commit's `elprovo.yml`, and the base commit's `elprovo.yml` when one exists and a selected proof runs on the base commit.

### 3.2 Runs

Elprovo replays the selected proofs one at a time, in file name order. For each proof, it alternates the commits: base run 1, head run 1, base run 2, head run 2, and so on. A proof that does not run on the base commit has head runs only.

Elprovo never runs two runs at the same time, so two apps never compete for the same ports.

One run of proof `P` on commit `C` has these steps:

1. Make a new directory for the run in the system's temporary directory. In it, make an empty directory named `elprovo-<run ID>`, the copy, where the run ID is 8 random characters from `[a-z0-9]`. Write the files of `C` into the copy, except `proofs/`, as a checkout of `C` writes them, with the repository's checkout filters such as Git LFS. The copy has no `.git` directory, no untracked or ignored files, and no submodule contents.
2. Write `proofs/` from the head commit into the copy, so that both commits run the same proof.
3. Take `elprovo.yml` from `C`. If `C` has none, take it from the head commit.
4. Make the run's empty home directory, temporary directory and artifacts directory in the run's directory, and the run's output directory, `<proof name>/<base|head>-<run number>/` in the replay's output directory. The logs go to the run's output directory.
5. Run `start`. If it exits with a status other than 0 or takes longer than `timeouts.start`, the run's outcome is **not run**. Go to step 8.
6. Run `ready` once a second until it exits 0. If it does not exit 0 within `timeouts.ready`, the outcome is **not run**. Go to step 8.
7. Run the proof's `run`. If it exits 0, the outcome is **pass**. If it exits with any other status, is ended by a signal, or takes longer than the proof's `timeout`, the outcome is **fail**.
8. If `stop` is set, run it, after every outcome. If `stop` exits with a status other than 0 or takes longer than `timeouts.stop`, the next run could not start clean. Elprovo then stops the whole replay, as section 3.5 says.
9. Move the artifacts to `artifacts/` in the run's output directory. When they are more than 1 GiB together, or Elprovo cannot read them all, delete them instead, and the report says so. Then delete the run's directory, with everything in it.

Docker Compose names a project after its directory by default, so each run gets its own Compose project without any setting.

### 3.3 Proof results

Elprovo gives each proof one result from its runs. It applies the first rule that matches:

| Rule | Head runs | Base runs | Result |
| --- | --- | --- | --- |
| 1 | At least one is not run | Any | `not-run` |
| 2 | Some pass and some fail | Any | `flaky` |
| 3 | All fail | Any | `failed` |
| 4 | All pass | None | `held` |
| 5 | All pass | At least one is not run | `not-run` |
| 6 | All pass | Some pass and some fail | `flaky` |
| 7 | All pass | All pass | `no-effect` |
| 8 | All pass | All fail | `verified` |

`verified` is the only result that shows what the change did. `held` shows that a claim holds on the head commit. It does not show that the proof can fail, so a reviewer reads such a proof with more care.

### 3.4 Exit status

Elprovo applies the first rule that matches:

| Rule | When | Status |
| --- | --- | --- |
| 1 | Elprovo could not replay: an option, a file or a revision is invalid, or a variable in `pass_env` is not set | 2 |
| 2 | The replay stopped early, as section 3.5 says | 2 |
| 3 | No proof is selected | 1 when `--require` is on, otherwise 0 |
| 4 | At least one proof is `failed`, `flaky` or `no-effect` | 1 |
| 5 | At least one proof is `not-run` | 2 |
| 6 | Every proof is `verified` or `held` | 0 |

### 3.5 Stopping early

Elprovo ends the replay early when `stop` fails, when it cannot make a run's directories, copy a commit or start `/bin/sh`, or when it gets `SIGINT` or `SIGTERM`. A failure that an interrupt causes counts as the interrupt. To end early:

1. It ends the command that runs, as section 4 says.
2. On an interrupt, it runs `stop`, if set, with its timeout.
3. It deletes the run's directories.
4. It writes the report for the runs that ended. The report marks the replay as stopped early, and gives no result for proofs whose runs did not all end.
5. It exits 2.

## 4. How commands run

- Each command runs as `/bin/sh -c '<command>'`, with the copy as its working directory, in its own process group.
- When a command takes longer than its timeout, or on an interrupt, Elprovo sends `SIGTERM` to its process group. When any process in the group still runs 10 seconds later, even after the shell has exited, Elprovo sends `SIGKILL` to the group. A command that ends in time can leave processes running, as a server started by `start` does. `stop` must end them.
- Each command gets only these environment variables, and a later item replaces an earlier one with the same name:
  1. `PATH`, and `USER` and `LANG` when they are set, from the environment of `elprovo replay`;
  2. `HOME` and `TMPDIR`, set to the run's own empty directories;
  3. the variables in `env`;
  4. the variables in `pass_env`, from the environment of `elprovo replay`;
  5. `ELPROVO_RUN_ID`, the run ID, and `ELPROVO_ARTIFACTS`, the absolute path of the run's empty artifacts directory.
- No variable, and no path in one, says which commit runs or whether it is the base or the head commit. A proof that inspects the code to decide its result defeats the replay, and a reviewer should reject it.
- The run's own `HOME` and `TMPDIR` keep tools from using the real ones by default. They are not a sandbox: commands run as the user who runs Elprovo, with that user's access to files, processes and the network. A command can still reach the real home directory by its path.
- Elprovo keeps the combined standard output and standard error of each command in a log file. After the run, it cuts a log longer than 10 MiB to 10 MiB, and adds a line that says so.
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

Elprovo prints one block for each proof, then a summary line. The [README](README.md) shows the form. For a `failed` or `flaky` proof, the block also shows the path of `proof.log` from the first head run that failed, or, when no head run failed, from the first base run that failed. For a `not-run` proof, it shows the reason and the log of the first run, in the order the runs ran, that did not run. The text form is for people. Programs read `report.json`.

### 5.3 `report.json`

```json
{
  "format": 1,
  "base": "3f2a1c0e9d8b7a6f5e4d3c2b1a0f9e8d7c6b5a49",
  "head": "9b7e4d2a1c0f9e8d7c6b5a493f2a1c0e9d8b7a6f",
  "runs": 2,
  "stopped_early": "",
  "proofs": [
    {
      "file": "proofs/coupon-once.yml",
      "claim": "A coupon applies once per order, even from two browser tabs.",
      "before": "fail",
      "claimed": true,
      "result": "verified",
      "base": [
        { "run": 1, "outcome": "fail", "exit_code": 1, "timed_out": false, "seconds": 41.2, "reason": "", "artifacts_dropped": false, "dir": "coupon-once/base-1" }
      ],
      "head": [
        { "run": 1, "outcome": "pass", "exit_code": 0, "timed_out": false, "seconds": 39.8, "reason": "", "artifacts_dropped": false, "dir": "coupon-once/head-1" }
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
| `stopped_early` | Why the replay ended early, such as `interrupted` or `stop exited with status 1`. It is empty when the replay ended normally. |
| `proofs[].claimed` | Whether the change adds or modifies the proof file. |
| `proofs[].result` | `verified`, `held`, `failed`, `flaky`, `no-effect` or `not-run`, from section 3.3. It is empty when the replay stopped before all the proof's runs ended. |
| `proofs[].base` | The base runs. The list is empty when the proof did not run on the base commit. |
| `outcome` | `pass`, `fail` or `not-run`, from section 3.2. |
| `exit_code` | The exit status of `run`, or `null` when `run` did not run or did not exit by itself. |
| `timed_out` | Whether `run` took longer than its timeout. |
| `seconds` | How long the run took, from the start of step 1 to the end of step 8. |
| `reason` | Why the outcome is `not-run`, such as `start exited with status 1` or `ready did not exit 0 within 5m`. It is empty otherwise. |
| `artifacts_dropped` | Whether Elprovo deleted the run's artifacts, because they were more than 1 GiB or it could not read them all. |
| `dir` | The run's output directory, relative to the replay's output directory. |

New fields may appear in a report of the same format. A program that reads reports must ignore fields it does not know.
