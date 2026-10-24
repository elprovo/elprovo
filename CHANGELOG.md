# Changelog

## Unreleased

- **Breaking:** Elprovo reads TOML files: `elprovo.toml` and `proofs/<name>.toml`, with the same fields as before. It no longer reads `elprovo.yml` or YAML proof files, because YAML changed some commands without notice. For example, it read `run: ! grep -q x f` as `grep -q x f`, and `claim: Issue #58 is fixed.` as `Issue`. TOML keeps every string as written.
- A value in `env` must be a TOML string, such as `PORT = "8080"`.
- A `.yml` or `.yaml` file directly in `proofs/` is an error, so that a proof file in the old format is never skipped without notice.
- Elprovo shows why a run failed. Each run in `report.json` has a new field, `last_line`: the last line that the proof, or `start` or `ready` for a `not-run` outcome, printed. The text output shows it for the run that it points to, such as the first base run of a `verified` proof. A base run that fails because a file or a tool is missing also counts as a failure, and this line makes that visible.
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
