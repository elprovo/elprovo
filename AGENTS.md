# Elprovo

Elprovo is a command line tool, `elprovo`, that runs the tests of a change on fresh copies of the code without the change and with it, and gives each test a verdict. [README.md](README.md) shows the usage, and [SPEC.md](SPEC.md) is the contract: the file Elprovo reads, the replay, the verdicts and `report.json`.

## Layout

| Path | Holds |
| --- | --- |
| `cmd/elprovo` | The program: signals, the version, and the call to `internal/cli` |
| `internal/cli` | Arguments, text output and exit status |
| `internal/replay` | Changed tests and packages, runs, process groups, verdicts and `report.json` |
| `internal/config` | Reading and checking `elprovo.toml` |
| `internal/gotest` | Reading `go test -json` output, and finding the tests in a Go file |
| `internal/gitrepo` | Git commands and the copy of a commit |
| `internal/testrepo` | Throwaway Git repositories and Go modules for tests only |
| `skills/elprovo-proofs` | The skill that tells coding agents how to write proofs |
| `elprovo.toml` | How `make replay` replays Elprovo's own tests |
| `assets/` | The README's demo: `demo-repo.sh` makes the demo repository, and `demo.tape` records `demo.gif` |

Imports go one way: `cmd/elprovo` → `cli` → `replay` → `config`, `gotest` and `gitrepo`. `cli` also uses types from `config` and `gotest`. Only tests import `testrepo`.

## Rules

- Run `make check` before you finish a change. It runs gofmt, go vet, `go mod tidy -diff`, the tests with the race detector, and the build. It is the gate. The CI workflow runs only by hand, on the elprovo organization's self-hosted Linux runners.
- A change to behavior that SPEC.md describes changes SPEC.md in the same commit. Keep the README examples equal to the real output.
- `report.json` and `elprovo.toml` are contracts. Add fields to `report.json`; do not rename or remove them without a new format. A new field in `elprovo.toml` needs a new format version.
- Test through `cli.Run` or the `replay` package, with real Git repositories from `testrepo`, real `/bin/sh`, real processes, and real `go test` for the verdicts. Do not mock Git, the shell or `go test`. Watch a new test fail before you trust it.
- Add a dependency only when it removes more risk than it adds. The only one today is `github.com/BurntSushi/toml`.
- When a change fixes a bug, add a test that fails without the fix. `make replay` must show it as `fixed`.
- Write documentation and messages in plain, short sentences.
