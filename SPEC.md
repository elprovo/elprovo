# Elprovo replay specification, version 3

This document defines the file that Elprovo reads, how `elprovo replay` and `elprovo reproduce` run the tests of a change, and what they report. The [README](README.md) shows the usage.

The words "must" and "must not" state requirements. A file or a command that breaks one is an error.

Elprovo runs on macOS and Linux. It needs `git`, `/bin/sh`, and the tools that the tests use, such as `go`. It does not support Windows: there, `elprovo replay` stops before it runs a command.

This release reads the results of Go tests only, from one Go module at the repository root.

## 1. `elprovo.toml`

`elprovo.toml` at the repository root says which files hold the tests, and which suites run them. It must be a [TOML 1.1](https://toml.io/en/v1.1.0) document of at most 64 KiB.

```toml
version = 3
pass_env = ["GOCACHE", "GOMODCACHE"]

[tests]
files = ["**/*_test.go", "**/testdata/**"]

[suites.unit]
run = "go test -json -count=1 -timeout=5m {packages}"
report = "go-test-json"

[suites.e2e]
run = "go test -json -count=1 -timeout=10m -tags=e2e ./e2e/..."
report = "go-test-json"
timeout = "20m"

[suites.e2e.app]
start = "docker compose up -d --build --wait"
ready = "curl -fsS http://localhost:8080/healthz"
stop = "docker compose down -v"
```

### 1.1 Fields

| Field | Required | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `version` | Yes | Integer | | The format version: `3`, or `2` as section 1.3 says. |
| `env` | No | Table of name to string | Empty | Plain values that every command gets. The file is in the repository, so these must not be secrets. |
| `pass_env` | No | Array of names | Empty | Variables that Elprovo copies from its own environment into every command, such as `GOCACHE` or `DOCKER_HOST`. A listed variable that is not set is an error. |
| `tests.files` | Yes | Array of patterns | | The files of the tests: the test files, and the files that the tests read, such as those in `testdata`. Go test files, `*_test.go`, always count, whether a pattern matches them or not. Section 1.2 defines the patterns. |
| `suites.<name>` | At least one | Table | | A suite: a command that runs tests, with what they need running. Suites run in the order the file lists them, as tables or as dotted keys. The name must match `[a-z0-9][a-z0-9-]*`, and must not be `artifacts`, the name of a run's artifacts directory. |
| `suites.<name>.run` | Yes | Command | | Runs the tests and prints what `go test -json` prints. When it contains `{packages}`, Elprovo puts the packages that the change touches there, as section 3.2 says. Without `{packages}`, it runs as written on every replay. |
| `suites.<name>.report` | Yes | String | | The format of what `run` prints. It must be `go-test-json`. |
| `suites.<name>.timeout` | No | Duration | `10m` | The longest time `run` may take. |
| `suites.<name>.app.start` | With an `app` | Command | | Starts what the suite's tests need, such as the app built from the copy, a database or other services. It must exit 0. |
| `suites.<name>.app.ready` | With an `app` | Command | | Exits 0 when the app can take requests. Elprovo runs it once a second until it exits 0. |
| `suites.<name>.app.stop` | No | Command | None | Stops the app and frees what it uses, such as containers, volumes and processes. Elprovo runs it after the suite in every run, also after a failure. |
| `suites.<name>.app.timeouts.start` | No | Duration | `10m` | The longest time `app.start` may take. |
| `suites.<name>.app.timeouts.ready` | No | Duration | `5m` | The longest time Elprovo waits for `app.ready` to exit 0. |
| `suites.<name>.app.timeouts.stop` | No | Duration | `2m` | The longest time `app.stop` may take. |

A suite of unit tests usually takes `{packages}` and has no `app`. A suite of e2e tests usually runs a fixed set of packages, such as `./e2e/...`, and has an `app`. Leave out a suite's `app` when its tests need nothing running.

Keep the suites apart: put the e2e tests behind a build tag, such as `//go:build e2e`, or in packages that the unit suite does not get. A test that two suites report counts once, as section 3.3 says, but it runs twice. When one of them skips it, the skip also hides a missing result in the other.

A name in `env` or `pass_env` must match `[A-Za-z_][A-Za-z0-9_]*` and must not start with `ELPROVO_`. A value in `env` must be a string, such as `PORT = "8080"`.

`pass_env` may name `HOME` or `TMPDIR`. The command then gets the real directory instead of the run's own empty one, as section 4 says. Do this only when a tool cannot work without it, because commands can then change files there. When Docker needs `DOCKER_HOST`, as with Colima, list it in `pass_env`.

`go test` keeps its build cache and its module cache in the home directory by default. Each run has an empty home directory, so without help every run builds everything again. List `GOCACHE` and `GOMODCACHE` in `pass_env`, and set them before you run Elprovo:

```sh
export GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)"
```

Give `go test` a `-timeout` that is shorter than the suite's `timeout`. Then a test that hangs fails with a message, and the tests that ended before it keep their results.

### 1.2 Values

- A **command** is a string that Elprovo runs with `/bin/sh -c`. Section 4 defines its directory and environment. TOML keeps a multi-line literal string, `'''…'''`, exactly as written, so it suits a command with several lines or with quotes.
- A **duration** is a whole number followed by `s`, `m` or `h`, such as `90s` or `10m`. It must be at least `1s` and at most `1h`.
- A **pattern** is a slash-separated path from the repository root. `*`, `?` and `[…]` match within one name, as Go's `path.Match` does. A `**` name matches any number of directories. At the end of a pattern, it matches everything in the directory before it. So `**/*_test.go` matches every Go test file, and `e2e/**` matches every file in `e2e`, but not a file named `e2e`.

### 1.3 Versions

- A version 3 reader rejects a field that this document does not define, so that a misspelled field fails instead of being ignored. Field names are case-sensitive: `RUN` is not `run`, and it is an error.
- Version 2 has one suite. `[tests]` holds its `run`, `report` and `timeout` next to `files`, and `[app]` holds its app. Elprovo reads it as one suite named `tests`, whose `run` must contain `{packages}`. Version 2 has no `[suites]`, and version 3 has no `tests.run`, `tests.report`, `tests.timeout` and no `[app]` at the top.
- Version 1 was the format of Elprovo 0.2, with proof files. Elprovo rejects an `elprovo.toml` of version 1 in the head commit, and asks for version 3. In the base commit, a version 1 file is not an error: the base runs use the head commit's file only, as section 3.2 says.
- A new field or a change in meaning comes with a new version. A release of Elprovo rejects a version it does not know.

## 2. Command line

```
elprovo reproduce [--base REV] [--head REV] [--runs N] [--out DIR] TEST_FILE...
elprovo replay [--base REV] [--head REV] [--runs N] [--require] [--out DIR] [--dry-run]
elprovo version
```

- `elprovo reproduce` checks the tests that reproduce a bug, before the fix: it runs the changed tests in each `TEST_FILE` on the base commit only, as section 3.7 says.
- `elprovo replay` compares the tests without the change and with it. It takes no arguments: it finds the changed tests itself.

Both must run inside a Git repository. They read commits, not the working tree, so they do not run uncommitted changes. Options come before `TEST_FILE` arguments.

| Option | Default | Meaning |
| --- | --- | --- |
| `--base REV` | The merge base of the head commit and `origin/HEAD` | The commit without the change. If `--base` is not given and `origin/HEAD` does not exist, Elprovo stops with an error that asks for `--base`. |
| `--head REV` | `HEAD` | The commit with the change. |
| `--runs N` | `2` | The number of runs on each commit. `N` must be from 1 to 10. |
| `--require` | Off | `replay` only. Treat a replay in which no test is `fixed` as a failure. Use it for a change that fixes a bug. |
| `--out DIR` | `.elprovo` | Where Elprovo writes logs, artifacts and the report. A relative path is relative to the working directory. |
| `--dry-run` | Off | `replay` only. Check the files and print the changed tests, without running any command. It exits 0 when the files are valid, 1 when no suite would run, as rule 3 of section 3.5 says, and `--require` is on, and 2 when a file is invalid. |

A `TEST_FILE` is a path relative to the working directory. It must end in `_test.go`, be in the head commit, and have at least one changed test, from section 3.1.

Elprovo resolves `--base` and `--head` to commit IDs once, before it runs anything.

## 3. Replay

### 3.1 Changed tests and packages

Elprovo lists the files that differ between the base and head commits, as `git diff --name-only --no-renames <base> <head>` lists them. A renamed file counts as deleted and added.

- The **packages** are the directories of the changed `.go` files, deleted ones included, such as `./cart`. Like `go test ./...`, Elprovo leaves out the files in `testdata` or `vendor`, the files whose path has a name that starts with `.` or `_`, and the directories of other Go modules: a directory with its own `go.mod` in the head commit, and every directory in it.
- A **changed test** is a test in a changed `_test.go` file that the head commit has. The test is changed when no test file of its package in the base commit has it, or declares it differently. So a test that moves to another file of its package does not change. Elprovo compares the declarations without comments, blank lines and spacing, so a change to a comment does not change a test. A base test file that does not parse adds no tests.
- `go test` runs two tests of one name in a package under that name, such as a test in package `x` and one in package `x_test`, or tests in files for different systems. Their results cannot be told apart, so a changed test whose name another test file of its package declares in the head commit is an error.
- A **test** is a top-level function `func TestXxx(t *testing.T)`, by the rules of `go test`. Its subtests count through it.
- The **other tests** are the tests that the runs report and that are not changed tests: for a suite with `{packages}`, the tests in the packages; for a suite without it, every test that it reports.

A suite with `{packages}` runs only when the change touches a package. A suite without it runs on every replay. When no suite would run, Elprovo runs nothing and writes no report. When a suite runs, the head commit must have a `go.mod` with a `module` line at the repository root.

Before it runs any command, Elprovo checks the head commit's `elprovo.toml`, the base commit's when it is of version 2 or 3, and the changed test files.

### 3.2 Runs

Elprovo alternates the commits: base run 1, head run 1, base run 2, head run 2, and so on. It never runs two runs at the same time, so two apps never compete for the same ports.

One run on commit `C` has these steps:

1. Make a new directory for the run in the system's temporary directory. In it, make an empty directory named `elprovo-<run ID>`, the copy, where the run ID is 8 random characters from `[a-z0-9]`. Write the files of `C` that are not test files into the copy: the files that do not end in `_test.go` and that the head commit's `tests.files` does not match. Write them as a checkout of `C` writes them, with the repository's checkout filters such as Git LFS. The copy has no `.git` directory, no untracked or ignored files, and no submodule contents.
2. Write the test files of the head commit into the copy. So both commits run the head commit's tests, and only the code differs between them.
3. Take `[tests]`, `env`, `pass_env` and each suite's command from the head commit's `elprovo.toml`, so that both commits run the tests in the same way. Take a suite's app from the `elprovo.toml` of `C` when that file is of version 2 or 3 and has an app for a suite of the same name, because the app belongs to the code. Version 2's `[app]` belongs to the suite `tests`. Otherwise take the head commit's app for the suite.
4. Make the run's empty home directory, temporary directory and artifacts directory in the run's directory, and the run's output directory, `<base|head>-<run number>/` in the replay's output directory. Each suite that runs writes its logs to `<suite>/` in the run's output directory.
5. Run each suite, in order. Leave out a suite with `{packages}` when the copy has none of the touched packages, so that it starts no app.
   1. If the suite has an app, run `app.start`. If it exits with a status other than 0 or takes longer than `app.timeouts.start`, the suite is **not run**. Go to step 5.4.
   2. If the suite has an app, run `app.ready` once a second until it exits 0. If it does not exit 0 within `app.timeouts.ready`, the suite is **not run**. Go to step 5.4.
   3. Run the suite's `run`. In a suite with `{packages}`, Elprovo replaces `{packages}` with the touched packages that have Go files in the copy, such as `./cart ./coupon`. A package whose name the shell would change is in single quotes. A suite without `{packages}` runs its command as written. Elprovo reads the `go test -json` events from the output, as section 3.3 says. The exit status of `run` decides nothing. If the output has no event, or Elprovo cannot read it, the suite is **not run**.
   4. If `app.stop` is set, run it, after every outcome. If `app.stop` exits with a status other than 0 or takes longer than `app.timeouts.stop`, the next suite or run could not start clean. Elprovo then stops the whole replay, as section 3.6 says.

   When a suite is not run, the run is **not run**, and the later suites of the run do not start.
6. Move the artifacts to `artifacts/` in the run's output directory. When they are more than 1 GiB together, or Elprovo cannot read them all, delete them instead, and the report says so. Then delete the run's directory, with everything in it.

A run that is not run gives no results, so Elprovo stops the replay after it, as section 3.6 says.

Docker Compose names a project after its directory by default, so each run gets its own Compose project without any setting.

### 3.3 Outcomes

In each run, each test gets one outcome from the `go test -json` events:

| Outcome | When |
| --- | --- |
| `passed` | The test passed. |
| `failed` | The test failed, also by a panic. A test that printed a panic and has no result also failed, as after `go test -timeout` or a panic in a goroutine. When one name has several results, as with `-count` or a test of the same name in package `x` and `x_test`, a failure counts over a pass, and a pass over a skip. |
| `skipped` | The test was skipped, such as with `t.Skip`. |
| `absent` | No suite printed a result for the test. Its package did not build, its `TestMain` exited, an earlier test stopped the package with a panic, `C` has no Go files in the package, or the suite's `run` did not end in time. |

When several suites print a result for one test in one run, a failure counts over a pass, and a pass over a skip.

An outcome other than `passed` comes with a message:

- for `failed` and `skipped`, the first line that the test printed, such as `cart_test.go:7: Total(100, 10) = 80; want 90`, or `panic: …`. When that line holds only a location, such as `cart_test.go:7:` from testify, the next lines follow it;
- for `absent`, the first line that says why the package failed, such as a compile error, or why Elprovo has no result. When a suite prints the test's result in any run, the first such suite, in the order of the file, is the test's own suite, and only its reason counts. So a unit suite that cannot build e2e tests, because they have a build tag, does not explain them. When the test's own suite did not end within its `timeout`, and gives the package no other reason, the message says so. For a test that no suite prints in any run, the reason comes first from a suite without `{packages}`, which chose the package itself, and then from any suite.

Elprovo removes the paths of the run's directories from the message, because they change in every run. A file in the copy gets its path from the repository root, and the run's home and temporary directories become `$HOME` and `$TMPDIR`. Then Elprovo keeps the first 200 characters of the message, and writes control characters as escape sequences, such as `\x1b` and `\n`.

`absent` is never `failed`. So a new test that cannot build without the change, because it uses code that only the head commit has, is `new` and not `fixed`.

### 3.4 Verdicts

Elprovo gives each test one verdict from its outcomes in all the runs. It applies the first rule that matches.

For a changed test:

| Rule | Head runs | Base runs | Verdict |
| --- | --- | --- | --- |
| 1 | Some are `failed` or `absent`, and some are `passed` | Any | `flaky` |
| 2 | Some are `failed` or `absent` | Any | `not-fixed` |
| 3 | All are `skipped` | Any | `skipped` |
| 4 | Any | All are `skipped` | `skipped` |
| 5 | Some are `skipped` | Any | `flaky` |
| 6 | All are `passed` | All are `failed` | `fixed` |
| 7 | All are `passed` | All are `passed` | `not-reproduced` |
| 8 | All are `passed` | All are `absent` | `new` |
| 9 | All are `passed` | Any other mix | `flaky` |

For another test:

| Rule | Runs | Verdict |
| --- | --- | --- |
| 1 | A head run is `failed` and another is `passed`, and no base run is `failed` | `flaky` |
| 2 | A head run is `failed`, and no base run is `failed` | `broke` |
| 3 | A run on the base commit is `absent` | `not-run` |
| 4 | Every base run is `passed`, and a head run is `absent` because its package failed, such as by a build error, a panic or a timeout | `broke` |
| 5 | A run on the head commit is `absent` | `not-run` |
| 6 | The runs on one commit have different outcomes | `flaky` |
| 7 | `skipped` on either commit | `skipped` |
| 8 | `passed` on both commits | `kept` |
| 9 | `failed` on both commits | `already-failing` |
| 10 | `failed` on the base commit and `passed` on the head commit | `fixed` |

Rules 1 and 2 come first, so that a test that fails with the change is never `not-run` or `skipped`. Both commits run the same test files, so in rule 4 the change stopped the test. A test that is `absent` on the head commit without a package failure gets `not-run`, because Elprovo cannot tell why it has no result.

`fixed` is the verdict that shows what the change did: the same test failed without the change and passed with it. Both commits run the head commit's tests, so only the code made the difference. A reviewer still reads the test, and the message of the base run, which the text output shows, to check that the test fails for the reason of the bug.

`not-reproduced` means that the test passed without the change too, so it does not show the bug. `new` means that the test could not run without the change. That is normal for a new feature, but it is not a reproduction.

### 3.5 Exit status

Elprovo applies the first rule that matches:

| Rule | When | Status |
| --- | --- | --- |
| 1 | Elprovo could not replay: an option, a file or a revision is invalid, or a variable in `pass_env` is not set | 2 |
| 2 | The replay stopped early, as section 3.6 says | 2 |
| 3 | No suite runs: the change touches no package, and every suite has `{packages}` | 1 when `--require` is on, otherwise 0 |
| 4 | At least one test is `not-fixed`, `not-reproduced`, `flaky` or `broke` | 1 |
| 5 | No test is `fixed`, and `--require` is on | 1 |
| 6 | Otherwise | 0 |

### 3.6 Stopping early

Elprovo ends the replay early when a run is not run, when a suite's `app.stop` fails, when it cannot make a run's directories, copy a commit or start `/bin/sh`, or when it gets `SIGINT` or `SIGTERM`. A failure that an interrupt causes counts as the interrupt. To end early:

1. It ends the command that runs, as section 4 says.
2. On an interrupt, it runs the running suite's `app.stop`, if set, with its timeout.
3. It deletes the run's directories.
4. It writes the report for the runs that ended. The report marks the replay as stopped early, and gives no verdict to any test.
5. It exits 2.

### 3.7 Reproduce

`elprovo reproduce` does only the base runs of section 3.2, with every suite, for the packages of the `TEST_FILE` arguments. Its copy is the base copy of a replay, with the head commit's tests, so a test that it calls `reproduced` fails in the base runs of a replay too.

It gives each changed test in the files a verdict. It applies the first rule that matches:

| Rule | Base runs | Verdict |
| --- | --- | --- |
| 1 | All are `failed` | `reproduced` |
| 2 | All are `passed` | `not-reproduced` |
| 3 | All are `absent` | `could-not-run` |
| 4 | All are `skipped` | `skipped` |
| 5 | Any other mix | `flaky` |

Its exit status is 2 when the input is invalid, the replay stopped early, or a test is `could-not-run`; 1 when a test is `not-reproduced`, `flaky` or `skipped`; and 0 when every test is `reproduced`.

## 4. How commands run

- Each command runs as `/bin/sh -c '<command>'`, with the copy as its working directory, in its own process group.
- When a command takes longer than its timeout, or on an interrupt, Elprovo sends `SIGTERM` to its process group. When any process in the group still runs 10 seconds later, even after the shell has exited, Elprovo sends `SIGKILL` to the group. A command that ends in time can leave processes running, as a server started by `app.start` does. `app.stop` must end them.
- Each command gets only these environment variables, and a later item replaces an earlier one with the same name:
  1. `PATH`, and `USER` and `LANG` when they are set, from the environment of `elprovo replay`;
  2. `HOME` and `TMPDIR`, set to the run's own empty directories;
  3. the variables in `env`;
  4. the variables in `pass_env`, from the environment of `elprovo replay`;
  5. `ELPROVO_RUN_ID`, the run ID, and `ELPROVO_ARTIFACTS`, the absolute path of the run's empty artifacts directory.
- No variable, and no path in one, says which commit runs or whether it is the base or the head commit. A test that inspects the code to decide its result defeats the replay, and a reviewer should reject it.
- The run's own `HOME` and `TMPDIR` keep tools from using the real ones by default. They are not a sandbox: commands run as the user who runs Elprovo, with that user's access to files, processes and the network. A command can still reach the real home directory by its path.
- Elprovo keeps the combined standard output and standard error of each command in a log file. After the run, it cuts a log longer than 10 MiB to 10 MiB, and adds a line that says so. It reads the events from `test.log` before it cuts it.
- Elprovo does not remove secrets from logs. Use test credentials only.

## 5. Output

### 5.1 Directory

Each replay writes to `<out>/runs/<replay ID>/`, where the replay ID is 8 random characters from `[a-z0-9]`:

```
<out>/runs/<replay ID>/
├── report.json
└── <base|head>-<run number>/
    ├── <suite>/
    │   ├── start.log
    │   ├── ready.log
    │   ├── test.log
    │   └── stop.log
    └── artifacts/
```

A log file exists only for a command that ran.

### 5.2 Text

Elprovo prints the commits, the packages and, when there are several, the suites. Then it prints:

1. a block for each changed test;
2. a block for each other test that is `fixed`, `broke`, `flaky`, `already-failing` or `not-run`, at most 20 of them;
3. when the replay stopped early, why, with the last line and the log of a run that was not run;
4. the number of tests with each verdict, for the changed tests and for the other tests;
5. the path of `report.json`.

A block shows the verdict, how the test ended on each commit, and the message of the run that explains the verdict. That run is the first one, in the order the runs ran, that is:

- `failed` on the base commit, for `fixed`;
- `absent` on the base commit, for `new`;
- `failed` or `absent` on the head commit, for `not-fixed` and `broke`;
- `failed` on the head commit, for `already-failing`;
- `failed` or `absent`, for `flaky`;
- `absent`, for `not-run`;
- `skipped`, for `skipped`.

For `not-fixed`, `flaky` and `broke`, the block also shows the path of that run's `test.log`, of the suite whose result or reason counted, or of the run's only suite when no suite did. When there are several suites, the lines about a run that was not run name the suite, and so do the progress lines that Elprovo prints to standard error. For `reproduce`, the explaining run is the first one that is `failed` for `reproduced`, and `absent` for `could-not-run`; the blocks show the base runs only, and one line counts the verdicts of the tests.

The text form is for people. Programs read `report.json`.

### 5.3 `report.json`

```json
{
  "format": 3,
  "command": "replay",
  "base": "3f2a1c0e9d8b7a6f5e4d3c2b1a0f9e8d7c6b5a49",
  "head": "9b7e4d2a1c0f9e8d7c6b5a493f2a1c0e9d8b7a6f",
  "runs": 1,
  "stopped_early": "",
  "suites": ["unit", "e2e"],
  "packages": ["./cart"],
  "changed": [
    {
      "package": "./cart",
      "name": "TestCouponAppliesOnce",
      "file": "cart/cart_test.go",
      "suites": ["unit"],
      "verdict": "fixed",
      "base": [{ "outcome": "failed", "message": "cart_test.go:7: Total(100, 10) = 80; want 90", "suite": "unit" }],
      "head": [{ "outcome": "passed", "message": "", "suite": "unit" }]
    }
  ],
  "regressions": [
    {
      "package": "./e2e",
      "name": "TestCheckout",
      "suites": ["e2e"],
      "verdict": "kept",
      "base": [{ "outcome": "passed", "message": "", "suite": "e2e" }],
      "head": [{ "outcome": "passed", "message": "", "suite": "e2e" }]
    }
  ],
  "base_runs": [
    {
      "run": 1, "outcome": "ran", "seconds": 9.6, "artifacts_dropped": false, "dir": "base-1",
      "suites": [
        { "suite": "unit", "outcome": "ran", "exit_code": 1, "timed_out": false, "seconds": 1.4, "reason": "", "last_line": "", "log": "base-1/unit/test.log" },
        { "suite": "e2e", "outcome": "ran", "exit_code": 0, "timed_out": false, "seconds": 8.2, "reason": "", "last_line": "", "log": "base-1/e2e/test.log" }
      ]
    }
  ],
  "head_runs": [
    {
      "run": 1, "outcome": "ran", "seconds": 9.4, "artifacts_dropped": false, "dir": "head-1",
      "suites": [
        { "suite": "unit", "outcome": "ran", "exit_code": 0, "timed_out": false, "seconds": 1.3, "reason": "", "last_line": "", "log": "head-1/unit/test.log" },
        { "suite": "e2e", "outcome": "ran", "exit_code": 0, "timed_out": false, "seconds": 8.1, "reason": "", "last_line": "", "log": "head-1/e2e/test.log" }
      ]
    }
  ]
}
```

The example shows one run on each commit to keep it short.

| Field | Meaning |
| --- | --- |
| `format` | The report format. It is `3`. |
| `command` | `replay` or `reproduce`. A `reproduce` report has no head runs and no other tests. |
| `base`, `head` | The full commit IDs. |
| `runs` | The number of runs on each commit. |
| `stopped_early` | Why the replay ended early, such as `interrupted`, `stop exited with status 1` or `base run 1 did not run: start exited with status 1`. With several suites, the reason starts with the suite's name, as in `base run 1 did not run: e2e: start exited with status 1` or `e2e: stop exited with status 1`. It is empty when the replay ended normally. |
| `suites` | The names of the suites, in the order they run. A version 2 file has one suite, `tests`. |
| `packages` | The packages, from section 3.1. |
| `changed` | The changed tests, in file and name order. |
| `regressions` | The other tests, in package and name order. |
| `package` | The test's package, as in `packages`. A whole suite's test may be in a package that is not in `packages`. |
| `name` | The test's name. |
| `file` | The file of a changed test. Other tests do not have it. |
| `suites` in a test | The suites that reported the test in any run, in suite order. |
| `verdict` | The verdict from section 3.4, or for `reproduce`, from section 3.7. It is empty when the replay stopped early. |
| `base`, `head` in a test | The test's outcome, its message and the suite whose result or reason counted, in each run on that commit, from section 3.3, in run order. `suite` is missing when no suite gave a result or a reason. |
| `base_runs`, `head_runs` | The runs on each commit, in run order. |
| `outcome` in a run | `ran`, or `not-run` when a suite did not run, from section 3.2. |
| `seconds` in a run | How long the run took, from the start of step 1 to the end of step 5. |
| `artifacts_dropped` | Whether Elprovo deleted the run's artifacts, because they were more than 1 GiB or it could not read them all. |
| `dir` | The run's output directory, relative to the replay's output directory. |
| `suites` in a run | The suites that the run started, in order. A suite after one that did not run is not in the list, and neither is a suite that step 5 left out. |
| `suite` | The suite's name. |
| `outcome` in a suite | `ran`, or `not-run` from section 3.2. |
| `exit_code` | The exit status of the suite's `run`, or `null` when it did not run or did not exit by itself. It decides nothing. |
| `timed_out` | Whether the suite's `run` took longer than its `timeout`. |
| `seconds` in a suite | How long the suite took, from the start of step 5.1 to the end of step 5.4. |
| `reason` | Why the suite is `not-run`, such as `start exited with status 1`, `ready did not exit 0 within 5m` or `the tests printed no go test -json events`. It is empty otherwise. |
| `last_line` | For a `not-run` suite, the last non-empty line of the log in `log`. Elprovo reads at most the last 64 KiB of the log, before it cuts a long log. It keeps the first 200 characters of the line, and then writes control characters as escape sequences. It is empty for a suite that ran, and when those 64 KiB hold no such line. |
| `log` | The log of the command that decided the suite's outcome, relative to the replay's output directory: `test.log`, or for a `not-run` suite, the log of `app.start`, `app.ready` or `run`. It is empty when no such command ran. |

New fields may appear in a report of the same format. A program that reads reports must ignore fields it does not know.
