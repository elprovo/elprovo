# Elprovo

Elprovo is a command line tool, `elprovo`, that replays the proofs of a change. [README.md](README.md) shows the usage, and [SPEC.md](SPEC.md) is the contract: the files Elprovo reads, the replay, the results and `report.json`.

## Layout

| Path | Holds |
| --- | --- |
| `cmd/elprovo` | The program: signals, the version, and the call to `internal/cli` |
| `internal/cli` | Arguments, text output and exit status |
| `internal/replay` | Proof selection, runs, process groups, results and `report.json` |
| `internal/config` | Reading and checking `elprovo.yml` and proof files |
| `internal/gitrepo` | Git commands and the copy of a commit |
| `internal/testrepo` | Throwaway Git repositories for tests only |
| `skills/elprovo-proofs` | The skill that tells coding agents how to write proofs |
| `elprovo.yml`, `proofs/` | Elprovo's own proofs, which `make replay` replays |

Imports go one way: `cmd/elprovo` → `cli` → `replay` → `config` and `gitrepo`. Only tests import `testrepo`.

## Rules

- Run `make check` before you finish a change. It runs gofmt, go vet, `go mod tidy -diff`, the tests with the race detector, and the build. It is the gate: the CI workflow runs only by hand while the repository is private.
- A change to behavior that SPEC.md describes changes SPEC.md in the same commit. Keep the README examples equal to the real output.
- `report.json` and the files Elprovo reads are contracts. Add fields to `report.json`; do not rename or remove them. A new field in `elprovo.yml` or a proof file needs a new format version.
- Test through `cli.Run` or the `replay` package, with real Git repositories from `testrepo`, real `/bin/sh` and real processes. Do not mock Git or the shell. Watch a new test fail before you trust it.
- Add a dependency only when it removes more risk than it adds. The only one today is `go.yaml.in/yaml/v3`.
- When a change adds or fixes behavior, add a proof in `proofs/`, as [skills/elprovo-proofs/SKILL.md](skills/elprovo-proofs/SKILL.md) says.
- Write documentation and messages in plain, short sentences.
