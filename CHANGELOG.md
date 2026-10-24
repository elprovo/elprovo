# Changelog

## 0.1.0 (not released yet)

The first release.

- `elprovo replay` replays the proofs that a change adds or modifies, on fresh copies of the base and head commits, and gives each proof a result: `verified`, `held`, `failed`, `flaky`, `no-effect` or `not-run`. [SPEC.md](SPEC.md) defines the files, the replay and `report.json`, version 1.
- Each run gets its own empty home directory and temporary directory, and only the environment variables that SPEC.md lists.
- `skills/elprovo-proofs` tells coding agents how to write proofs and replay them.
- It runs on macOS and Linux. It does not support Windows.
- A copy is written as a checkout writes it, with the repository's checkout filters, such as Git LFS.
