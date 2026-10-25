# Changelog

## Unreleased

- `elprovo.toml` version 3 has named suites, `[suites.<name>]`, each with its own `run`, `report`, `timeout` and `app`. Suites run one after the other in each run, in the order the file lists them. `[tests]` keeps only `files`.
- A suite whose `run` has `{packages}` gets the packages that the change touches, as before, and is left out when there are none. A suite without `{packages}`, such as an e2e suite, runs as written on every replay, and all its tests count. So a change to the app's code alone shows whether an e2e test broke.
- When several suites report one test in one run, a failure counts over a pass, and a pass over a skip. A test without a result takes its reason from the suite that runs its package's tests, so a unit suite that cannot build an e2e package does not explain the e2e tests.
- A suite whose app does not start stops the replay with exit status 2, and the message names the suite.
- Version 2 still works, as one suite named `tests`. A base run takes each suite's `app` from the base commit's file, of version 2 or 3, when it has one for that suite.
- **Breaking:** `report.json` is format 3. It lists the `suites`. Each run has `suites`, with the outcome, exit code, reason, last line and log of each suite. Each test has `suites`, and each of its results has the `suite` whose result counted. Logs are in `<side>-<n>/<suite>/`.

## 0.3.1 (2026-10-25)

Elprovo is public, at `github.com/elprovo/elprovo`. Nothing in its behavior changed.

- The module path is `github.com/elprovo/elprovo`. Install with `go install github.com/elprovo/elprovo/cmd/elprovo@latest`. Earlier tags have the old module path, so `go install` cannot use them.
- Each release has archives for macOS and Linux, on amd64 and arm64, built with GoReleaser.
- CI and the pull request check run on GitHub-hosted runners.

## 0.3.0 (2026-10-24)

A bug fix now proves itself with its own test: the test fails without the fix and passes with it, and the other tests keep their results.

- **Breaking:** the tests are the proofs. Elprovo runs the Go tests of the packages that a change touches, on fresh copies of the base and head commits, and gives each test a verdict. Proof files, `proofs/`, `claim`, `before`, `--all` and `PROOF` arguments are gone.
- **Breaking:** `elprovo.toml` is version 2. `[tests]` says which files hold the tests and how to run them, and the optional `[app]` table holds `start`, `ready` and `stop`. Elprovo rejects version 1 in the head commit with a message that asks for version 2. In the base commit, a version 1 file gives way to the head commit's.
- **Breaking:** `report.json` is format 2, with `command`, and the lists `changed`, `regressions`, `base_runs` and `head_runs`.
- To move from 0.2: write `elprovo.toml` version 2, turn each proof into a Go test, and delete `proofs/`.
- `elprovo reproduce TEST_FILE...` checks a reproduction before the fix. It runs the changed tests in the files on the base commit only, and calls each `reproduced`, `not-reproduced`, `could-not-run`, `flaky` or `skipped`.
- Both copies run the head commit's tests, as the head commit's `[tests]`, `env` and `pass_env` say, so only the code differs between them. A base run starts the base commit's `[app]` when it has one. Every `*_test.go` file is a test file.
- A changed test whose name another test file of its package declares, such as in package `x_test`, is an error, because `go test` runs both under one name.
- A changed test is `fixed`, `not-reproduced`, `new`, `not-fixed`, `flaky` or `skipped`. The other tests in the packages are `kept`, `broke`, `already-failing`, `not-run`, `flaky`, `skipped` or `fixed`.
- A test without a result is `absent`, never `failed`. So a test that does not build without the change is `new`, not `fixed`. A test that printed a panic and has no result failed, as after `go test -timeout` or a panic in a goroutine.
- Another test that fails with the change and never without it `broke`, and so does one that passed without the change and has no result with it because its package failed.
- A test that moves to another file of its package, or gets a comment or a blank line, is not changed.
- `--require` makes a replay in which no test is `fixed` fail.
- A run whose app does not start, or whose tests print no events, stops the replay with exit status 2. The test command's own exit status decides nothing.
- Each verdict comes with the message of the run that explains it, such as the line that a test failed with on the base commit. Paths in the copy are relative to the repository root, and the run's home and temporary directories are `$HOME` and `$TMPDIR`. A testify message gets the lines after its location.
- This release reads `go test -json` output only, from one Go module at the repository root. It leaves out the packages of nested modules.
- The demo repository, the README and `assets/demo.gif` show the three steps: reproduce the bug, fix it, replay.
- `skills/elprovo-proofs` is now `skills/elprovo-reproduce`. It tells agents to commit the test first, run `elprovo reproduce`, fix, and run `elprovo replay --require`.
- The `elprovo` workflow replays each pull request with `elprovo` built from the base commit, and posts the verdicts on the pull request, with a headline such as "Reproduced on main and fixed here. Nothing else broke."
- `make replay` replays Elprovo's own Go tests.

## 0.2.0 (2026-10-24)

- **Breaking:** Elprovo reads TOML files: `elprovo.toml` and `proofs/<name>.toml`, with the same fields as before. It no longer reads `elprovo.yml` or YAML proof files, because YAML changed some commands without notice. For example, it read `run: ! grep -q x f` as `grep -q x f`, and `claim: Issue #58 is fixed.` as `Issue`. TOML keeps every string as written.
- Field names are case-sensitive. An unknown field, such as `CLAIM`, is an error, and so is an `env` that is not a table.
- A value in `env` must be a TOML string, such as `PORT = "8080"`.
- A `.yml` or `.yaml` file directly in `proofs/` is an error, in any case, and so is a `.TOML` file. So a proof file in the old format or spelling is never skipped without notice.
- Elprovo shows why a run failed. Each run in `report.json` has a new field, `last_line`: the last line that the proof printed, or, for a `not-run` outcome, the last line that `start` or `ready` printed. The text output shows it for the run that it points to, such as the first base run of a `verified` proof. A base run that fails because a file or a tool is missing also counts as a failure, and this line makes that visible.
- `skills/elprovo-proofs` and the README tell agents to check the line that each base run ended with, to keep every file a proof needs in `proofs/`, and to point to the CI check instead of pasting a local summary as the evidence.
- The README is rewritten around a demo repository that `assets/demo-repo.sh` makes, with real output, and a recording of a replay, `assets/demo.gif`.
- The README's CI job keeps `report.json` and the logs as an artifact, and says to make the job a required status check. Elprovo's own workflow keeps them too.
- The tests now pass on Linux too: Ubuntu 24.04 on arm64, in CI on the elprovo organization's self-hosted runners, where `/bin/sh` is dash.
- A test replays a repository that uses Git LFS.

## 0.1.0 (2026-10-24)

The first release.

- `elprovo replay` replays the proofs that a change adds or modifies, on fresh copies of the base and head commits, and gives each proof a result: `verified`, `held`, `failed`, `flaky`, `no-effect` or `not-run`. [SPEC.md](SPEC.md) defines the files, the replay and `report.json`, version 1.
- Each run gets its own empty home directory and temporary directory, and only the environment variables that SPEC.md lists.
- `skills/elprovo-proofs` tells coding agents how to write proofs and replay them.
- It is built for macOS and Linux. It does not support Windows.
- A copy is written as a checkout writes it, with the repository's checkout filters, such as Git LFS.

Known limits of this release:

- The tests ran on macOS only. Linux builds and passes `go vet`, but its tests have not run yet.
- The tests check that a copy applies checkout filters. Git LFS itself was not installed, so a repository that uses it is untested.
