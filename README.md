# Elprovo

Elprovo shows that a bug fix fixes the bug. The fix comes with a test that reproduces the bug. Elprovo runs that test on a fresh copy of the code without the fix and on a fresh copy with it, and shows that the test failed first and then passed. It also runs the other tests in the packages that the fix touches, and the e2e tests of the whole app, and shows that they kept their results.

![elprovo reproduce shows that a new test fails on main, and elprovo replay shows that the fix makes it pass](assets/demo.gif)

**Status:** 0.4.0. It reads Go tests only. It runs on macOS and Linux. It does not support Windows, because it runs commands with `/bin/sh` in Unix process groups. [SPEC.md](SPEC.md) is the contract: the file, the runs, the verdicts and `report.json`. [CHANGELOG.md](CHANGELOG.md) lists what each version changed.

## Why

A pull request says "fixed, and the tests pass". That sentence costs nothing to write, whether a person or a coding agent writes it. Passing tests show that the code runs. They do not show that the change fixed the bug: a test can pass with the change and without it.

The check that does show it starts from the bug:

1. Write a test that fails because of the bug.
2. Run it on the code without the fix. It must fail, for the reason of the bug.
3. Fix the bug. Run the test on the code with the fix. It must pass.
4. Run the other tests on both. They must keep their results.

Benchmarks for coding agents, such as SWE-bench, grade a fix in the same way. Elprovo runs these checks on copies that the author's machine never touched, twice on each commit, and alternates the commits to catch flaky results. The reviewer then reads one test and the line it failed with, and spends the rest of the review on the code.

## Install

Elprovo needs `git` and `/bin/sh`. Download an archive for macOS or Linux from the [releases](https://github.com/elprovo/elprovo/releases), or install it with Go 1.27 or later:

```console
$ go install github.com/elprovo/elprovo/cmd/elprovo@latest
```

## Try it

The demo is in this repository, so clone it first. Its e2e tests call the shop's API in Docker, so they need Docker with Compose. Each run has an empty home directory, so give `go test` its caches, and `docker` its settings, through the environment:

```console
$ git clone https://github.com/elprovo/elprovo.git
$ cd elprovo
$ export GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" DOCKER_CONFIG="$HOME/.docker"
```

[`assets/demo-repo.sh`](assets/demo-repo.sh) makes a demo repository and prints its path. It holds a small shop API whose `SAVE10` coupon takes 10% off again on every use. It has unit tests in `./cart`, and e2e tests in `./e2e` that call the API of the shop in Docker. Its branches follow the steps above:

```console
$ cd "$(sh assets/demo-repo.sh)"
$ git log --oneline --graph --all
* a0a8535 (api-break) Take the coupon code from the parameter coupon
| * 5ceb1a5 (coupon-once) Apply a coupon once per cart
| | * ce6c064 (late-check) Apply a coupon once per cart, with the check after the discount
| |/
| * b1f82c2 (HEAD -> test-only) Reproduce: SAVE10 takes 10% off again on every use
|/
* 5b07b34 (main) Add the shop API
```

### 1. Reproduce the bug

The branch `test-only` adds one test, `cart/coupon_test.go`, and no fix:

```go
// SAVE10 took 10% off again on every use, so two uses took 19% off.
func TestCouponAppliesOnce(t *testing.T) {
	var c Cart
	c.Add(10000)
	c.ApplyCoupon("SAVE10")
	c.ApplyCoupon("SAVE10")
	if got := c.Total(); got != "90.00" {
		t.Errorf("total after SAVE10 twice is %s; want 90.00", got)
	}
}
```

Check that it fails on `main`:

```console
$ elprovo reproduce --base main cart/coupon_test.go
base 5b07b34  head b1f82c2  2 runs on the base commit
packages: ./cart
suites: unit, e2e

✓ reproduced      TestCouponAppliesOnce  cart/coupon_test.go
                  base: failed 2 of 2
                  base run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00

Tests: 1 reproduced.
Report: .elprovo/runs/3l8rz63k/report.json
```

`reproduced` means that the test failed in every run on `main`. Read the line under it: it must name the bug. A test that fails for another reason, such as a missing file, reproduces nothing. A test that does not build without the fix is `could not run`, and a test that passes is `not reproduced`.

The demo has two suites, `unit` and `e2e`, and each run runs both, as [elprovo.toml](#elprovotoml) says. Elprovo also prints a line for each run to standard error, such as `base run 1: unit ran (0.4s), e2e ran (3.5s)`.

### 2. Fix it, and replay

The branch `coupon-once` adds the fix on top of the test:

```console
$ git switch -q coupon-once
$ elprovo replay --base main
base 5b07b34  head 5ceb1a5  2 runs each
packages: ./cart
suites: unit, e2e

✓ fixed           TestCouponAppliesOnce  cart/coupon_test.go
                  base: failed 2 of 2   head: passed 2 of 2
                  base run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00

Changed tests: 1 fixed.
Other tests: 4 kept.
Report: .elprovo/runs/2p4d4p79/report.json
```

`fixed` means that the test failed on `main` and passed with the fix, in every run. Both copies ran the same test, so only the code made the difference. `4 kept` means that the other two tests in `./cart` and the two e2e tests passed on both commits, so the fix broke nothing there.

`elprovo replay --dry-run` lists the changed tests, and runs nothing:

```console
$ elprovo replay --base main --dry-run
base 5b07b34  head 5ceb1a5  2 runs each (dry run)
packages: ./cart
suites: unit, e2e

changed  TestCouponAppliesOnce  cart/coupon_test.go

Dry run: no command ran.
```

### 3. A fix that does not fix

On the branch `late-check`, the fix checks the coupon only after it takes the discount. The test fails with the fix too:

```console
$ git switch -q late-check
$ elprovo replay --base main
base 5b07b34  head ce6c064  2 runs each
packages: ./cart
suites: unit, e2e

✗ not fixed       TestCouponAppliesOnce  cart/coupon_test.go
                  base: failed 2 of 2   head: failed 2 of 2
                  head run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00
                  log: .elprovo/runs/ik5qgt26/head-1/unit/test.log

Changed tests: 1 not fixed.
Other tests: 4 kept.
Report: .elprovo/runs/ik5qgt26/report.json
```

`elprovo replay` exits 1, and the output points to the log of the run that failed. `report.json` keeps every run. The changed test of this replay:

```json
{ "package": "./cart", "name": "TestCouponAppliesOnce", "file": "cart/coupon_test.go", "suites": ["unit"], "verdict": "not-fixed",
  "base": [{ "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00", "suite": "unit" }, { "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00", "suite": "unit" }],
  "head": [{ "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00", "suite": "unit" }, { "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00", "suite": "unit" }] }
```

Add `.elprovo/` to your `.gitignore`.

### 4. A change that breaks the API

On the branch `api-break`, a change to `main.go` takes the coupon code from the query parameter `coupon` instead of `code`. No unit test covers `main.go`, so only the e2e tests can see it:

```console
$ git switch -q api-break
$ elprovo replay --base main
base 5b07b34  head a0a8535  2 runs each
packages: .
suites: unit, e2e

✗ broke           TestCheckoutWithACoupon  ./e2e
                  base: passed 2 of 2   head: failed 2 of 2
                  head run 1: shop_test.go:38: POST /cart/coupon?code=SAVE10: 404 Not Found; want 200
                  log: .elprovo/runs/zgha49oz/head-1/e2e/test.log

Changed tests: none.
Other tests: 1 kept, 1 broke.
Report: .elprovo/runs/zgha49oz/report.json
```

The change touches no e2e test and not the package `./e2e`. The e2e suite runs on every replay anyway, so its tests count as other tests. `broke` means that the test passed without the change and failed with it. `elprovo replay` exits 1.

## How a replay works

`elprovo replay` compares the base commit with the head commit:

- The **changed tests** are the `TestXxx` functions that the change adds, or edits in more than comments and spacing.
- The **packages** are the directories of the Go files that the change adds, modifies or deletes.
- The **other tests** are the tests in those packages that the change does not touch, and every test of a suite that runs on every replay, such as the e2e tests.

One run on a commit has four steps:

1. Elprovo writes the commit's code into a new directory, as a checkout writes it, and the head commit's test files beside it. So both commits run the same tests, in the same way, and only the code differs.
2. It gives the run an empty home directory and an empty temporary directory.
3. It runs each suite in turn. It starts the suite's app, if it has one, runs the suite's test command, such as `go test -json`, reads the result of each test, and stops the app.
4. It deletes the directory.

It alternates the commits, two runs each by default: base 1, head 1, base 2, head 2. It never does two runs at the same time. `elprovo reproduce` does only the base runs, for the files that you name.

The base commit is the merge base with `origin/HEAD`, unless you give `--base`. Elprovo reads commits, not the working tree, so commit a test before you reproduce it. `elprovo replay -h` lists every option.

## Read the verdicts

In each run, each test passed, failed, was skipped, or is **absent**: the run printed no result for it, because its package did not build, its `TestMain` exited, or an earlier test panicked. A test that hangs until `go test -timeout`, or panics in a goroutine, failed. For a changed test, absent never counts as failed.

| Changed test | What the runs did |
| --- | --- |
| `fixed` | Failed without the change and passed with it, in every run |
| `not reproduced` | Passed without the change too, so it does not show the bug |
| `new` | Was absent without the change, such as a test of a new function, and passed with it |
| `not fixed` | Failed or was absent with the change |
| `flaky` | Gave different results in different runs on the same commit |
| `skipped` | Was skipped on either commit |

| Other test | What the runs did |
| --- | --- |
| `kept` | Passed on both commits |
| `broke` | Failed with the change and never without it, or passed without the change and stopped with it because its package failed, such as by a build error |
| `already failing` | Failed on both commits |
| `not run` | Was absent on a commit for another reason, so Elprovo cannot compare it |
| `fixed` | Failed without the change and passed with it |
| `flaky`, `skipped` | As for a changed test |

`elprovo replay` exits 1 when a test is `not fixed`, `not reproduced`, `flaky` or `broke`, and 0 otherwise. With `--require`, it also exits 1 when no test is `fixed`, so use `--require` for a change that fixes a bug. It exits 2 when it could not replay, such as when the tests printed no results. `elprovo reproduce` exits 0 only when every test is `reproduced`, 2 when a test could not run, and 1 otherwise.

Programs read `report.json`, which [SPEC.md](SPEC.md) describes.

## elprovo.toml

The repository says once how to run its tests, in `elprovo.toml` at its root. It is [TOML](https://toml.io/en/v1.1.0). The demo's file:

```toml
version = 3
pass_env = ["GOCACHE", "GOMODCACHE", "DOCKER_CONFIG"]

[tests]
files = ["**/*_test.go", "**/testdata/**"]

[suites.unit]
run = "go test -json -count=1 -timeout=5m {packages}"
report = "go-test-json"

[suites.e2e]
run = "go test -json -count=1 -timeout=5m -tags=e2e ./e2e"
report = "go-test-json"

[suites.e2e.app]
start = "docker compose up --detach --build --wait"
ready = "curl -fsS http://localhost:8080/healthz"
stop = "docker compose down --volumes --rmi local"
```

| Field | Default | What it does |
| --- | --- | --- |
| `version` | | The format version. It must be `3`, or `2`, the format of Elprovo 0.3, which has one suite. |
| `tests.files` | | The files of the tests: the test files, and the files that they read. Go test files, `*_test.go`, always count. The base copy gets the head commit's version of these files. `**` matches any number of directories. |
| `suites.<name>` | | A suite: tests that run with one command and one setup, such as `unit` or `e2e`. Each run runs the suites in the order of the file. A name has lowercase letters, digits and `-`, and is not `artifacts`. |
| `suites.<name>.run` | | The test command. It must print `go test -json` output. When it has `{packages}`, Elprovo puts the packages there, such as `./cart`, and the suite runs only when the change touches a package. Without `{packages}`, the command runs as written on every replay, and all its tests count. |
| `suites.<name>.report` | | The format of that output. It must be `go-test-json`. |
| `suites.<name>.timeout` | `10m` | The longest time the test command may take. Give `go test` a shorter `-timeout`, so that a test that hangs fails with a message. |
| `suites.<name>.app` | none | For tests that need a running app, such as the app built from the copy and its database: `start`, `ready` and `stop` commands, and `timeouts.start`, `timeouts.ready` and `timeouts.stop`. In each run, Elprovo runs `start`, waits until `ready` exits 0, runs the suite's tests, and then runs `stop`. A base run uses the base commit's app for the suite when it has one, because the app belongs to the code. |
| `env` | empty | A table of plain values that every command gets, such as `env.PORT = "8080"`. Not for secrets. |
| `pass_env` | empty | Names of variables that Elprovo copies from its own environment, such as `GOCACHE`. A listed variable that is not set is an error. |

Put the files that the tests read, such as `testdata`, in `tests.files`. Otherwise a new test fails without the change only because its data file is missing, and that failure is not the bug.

Keep the suites apart, so that no test runs twice. The demo's e2e tests have the build tag `e2e`, so only the e2e suite runs them. When a change touches `./e2e`, the unit suite cannot build that package, and that failure does not explain the e2e tests.

Field names are case-sensitive, and a field that SPEC.md does not define is an error, so a misspelled field fails instead of being ignored. Elprovo's own [`elprovo.toml`](elprovo.toml) is of version 2, with one suite and no app.

## In CI

The reviewer should trust a replay that their own CI ran, not a summary that the author pasted. The Elprovo action replays the tests of a pull request in GitHub Actions. It puts the output in the job summary and on the pull request, and keeps the report and the logs as an artifact:

```yaml
name: elprovo
on: pull_request

permissions:
  contents: read
  pull-requests: write

concurrency:
  group: elprovo-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
  replay:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - uses: elprovo/elprovo@v0.4.0
```

The step ends with the exit status of `elprovo replay`. Make the job a required status check of the branch, so that a pull request merges only when the replay passes. On a private repository, GitHub offers required checks only on a paid plan.

| Input | Default | What it does |
| --- | --- | --- |
| `base` | The merge base of `HEAD` and `origin/<base branch>` on a pull request | The commit without the change. The default needs the whole history, `fetch-depth: 0`. |
| `require` | `false` | Fails when no test is `fixed`, as `--require` does. |
| `runs` | `2` | The number of runs on each commit. |
| `working-directory` | `.` | The repository's directory in the workspace. |
| `elprovo` | | An `elprovo` binary to run instead, as a name on `PATH` or a path from `working-directory`. By default, the action builds `elprovo` from its own source with the job's `go`. When Elprovo needs a newer Go, `go` downloads it for that build only, so the job's Go does not change. |
| `comment` | `true` | Posts the result on the pull request, in one comment for each job and working directory, and updates it on each run. A pull request from a fork gets a token that cannot comment, so the action then only warns. |
| `token` | `github.token` | The token that posts the comment. It needs `pull-requests: write`. The action updates its earlier comment only when a bot posted it, as with `github.token`. |

The outputs are `exit-status`, and `report`, the path of `report.json`. Each run has an empty home directory, so the action sets these variables for the replay:

- `GOCACHE` and `GOMODCACHE`, from the job's `go env`, so that `pass_env` can pass Go's caches.
- `DOCKER_CONFIG`, set to `~/.docker` when `docker` is installed and `DOCKER_CONFIG` is not set, so that `pass_env` can pass Docker's settings: its context, its plugins such as Compose, and its registry logins.
- `TMPDIR`, set to the runner's temporary directory, where Elprovo makes its copies. It is in the runner's work directory. On a runner that uses Docker in Docker, such as one of Actions Runner Controller, the Docker daemon sees only that directory, so a bind mount from the copy works.

A GitHub-hosted Ubuntu runner has Docker and Docker Compose, so an e2e suite can build and start the app from each copy with `docker compose`. Each run then has its own app and data, and the suite's `app.stop` removes them. A service container of the job, under `services:`, lives for the whole job, so every run shares it. A suite that uses one must reset its data in `app.start`, such as by creating the database again.

An e2e suite runs on every replay, twice on each commit by default. With `runs: 1`, a replay takes half the time, but it cannot find flaky tests. Do not limit a required check to some `paths`: GitHub keeps the check of a skipped workflow pending, so the pull request cannot merge.

Elprovo's own [pull request check](.github/workflows/elprovo.yml) builds `elprovo` from the base commit and gives it to the action, so a pull request cannot change the `elprovo` that judges it. A pull request can still change the workflow file, `action.yml`, `go.mod` and `elprovo.toml`, so review changes to them. Its tests also run in the job, as the job's user, so a hostile change can make its own check pass. The check catches honest mistakes. Review the tests too.

## For coding agents

Add this to the repository's `AGENTS.md`:

> To fix a bug, first write a Go test that fails because of the bug, and commit it alone. Run `elprovo reproduce <test file>`, and go on only when it says `reproduced` and the line under it names the bug. Then make the smallest fix, commit it, and run `elprovo replay --require`. In your report, name the test and point to the CI check that replays it. If there is no such check, paste the local summary and say that it ran locally.

For agents that load skills, such as Claude Code, [skills/elprovo-reproduce/SKILL.md](skills/elprovo-reproduce/SKILL.md) gives the same rule with the steps.

## What a run can see

Each command runs in the copy, with `/bin/sh -c`, and gets only these environment variables:

- `PATH`, `USER` and `LANG` from your environment;
- `HOME` and `TMPDIR`, set to empty directories that belong to the run;
- the values in `env`, and the variables that `pass_env` names;
- `ELPROVO_RUN_ID`, and `ELPROVO_ARTIFACTS`, an empty directory for traces and screenshots. After the run, Elprovo moves what is there to the run's directory in `.elprovo`.

So tools that read or write the home directory use the run's own, not yours. No variable says which commit runs. Three things follow from the empty home directory:

- Tools keep caches in the home directory, so each run starts with empty caches, unless `pass_env` names them, as `GOCACHE` and `GOMODCACHE` above.
- Tool managers such as mise and asdf find tools through the home directory, so their shims can fail in a run. Put the real tool directory first in `PATH`, or name what the tool needs in `pass_env`.
- Docker keeps its settings in `~/.docker`: the current context, which says how to reach the daemon, plugins such as Compose, and registry logins. If `docker` or `docker compose` fails in a run, set `DOCKER_CONFIG` to that directory and add `DOCKER_CONFIG` to `pass_env`, as the demo does.

`pass_env` can also name `HOME`. Do that only when a tool cannot work without your real home, because commands can then change files there.

## Security

A run is not a sandbox. Commands run as you, with your access to files, processes and the network, so a command can still reach your real home by its path. Elprovo runs the code of the change it replays: its tests, its test commands and the commands of its apps. Replay only changes whose code you would run yourself.

- In CI, replay pull requests from forks only in a job that has no secrets and a read-only token, such as a GitHub Actions job on `pull_request`.
- Variables in `pass_env` reach every command of every run, and the logs keep what the commands print. Pass test credentials only.

## What Elprovo does not do

- It does not write tests or fixes.
- It does not judge whether a test reproduces the right bug. Read the test, and the line that its base run failed with.
- It does not prove that a change is correct. It shows that a test fails without the change and passes with it, and that the other tests in the same packages kept their results. A weak test shows little, and the unit tests of other packages do not run.
- It reads Go tests only, from one Go module at the repository root. It leaves out the packages of nested modules, as `go test ./...` does.
- It gives verdicts to top-level test functions. A subtest, or a method of a testify suite, counts only through its top-level function.
- It does not isolate a run beyond the environment, the home directory and the temporary directory. Docker images, caches, the network and anything outside the copy are shared.

## License

MIT. See [LICENSE](LICENSE).
