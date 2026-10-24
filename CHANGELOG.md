# Changelog

## Unreleased

- **Breaking:** the tests are the proofs. Elprovo runs the tests of a change on fresh copies of the base and head commits, and gives each test a verdict. Proof files, `proofs/`, `claim`, `before`, `--all` and `PROOF` arguments are gone.
- **Breaking:** `elprovo.toml` is version 2. `[tests]` says which files hold the tests and how to run them, and the optional `[app]` table holds `start`, `ready` and `stop`. Elprovo rejects version 1 with a message that asks for version 2. This release reads `go test -json` output only.
- Both copies run the head commit's tests, so only the code differs between them.
- A changed test is `fixed`, `not-reproduced`, `new`, `not-fixed`, `flaky` or `skipped`. The other tests in the packages that the change touches are `kept`, `broke`, `already-failing`, `not-run`, `flaky`, `skipped` or `fixed`.
- A test without a result is `absent`, never `failed`. So a test that does not build without the change is `new`, not `fixed`.
- `--require` makes a replay in which no test is `fixed` fail.
- `elprovo reproduce TEST_FILE...` checks a reproduction before the fix: it runs the changed tests in the files on the base commit only, and calls each `reproduced`, `not-reproduced`, `could-not-run`, `flaky` or `skipped`.
- **Breaking:** `report.json` is format 2, with the lists `changed`, `regressions`, `base_runs` and `head_runs`.
- A run whose app does not start, or whose tests print no events, stops the replay with exit status 2.
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
