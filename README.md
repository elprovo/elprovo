# Elprovo

Elprovo shows that a bug fix fixes the bug. The fix comes with a test that reproduces the bug. Elprovo runs that test on a fresh copy of the code without the fix and on a fresh copy with it, and shows that the test failed first and then passed. It also runs the other tests in the packages that the fix touches, and shows that they kept their results.

![elprovo reproduce shows that a new test fails on main, and elprovo replay shows that the fix makes it pass](assets/demo.gif)

**Status:** 0.3.0. It reads Go tests only. It runs on macOS and Linux. It does not support Windows, because it runs commands with `/bin/sh` in Unix process groups. [SPEC.md](SPEC.md) is the contract: the file, the runs, the verdicts and `report.json`. [CHANGELOG.md](CHANGELOG.md) lists what each version changed.

## Why

A pull request says "fixed, and the tests pass". That sentence costs nothing to write, whether a person or a coding agent writes it. Passing tests show that the code runs. They do not show that the change fixed the bug: a test can pass with the change and without it.

The check that does show it starts from the bug:

1. Write a test that fails because of the bug.
2. Run it on the code without the fix. It must fail, for the reason of the bug.
3. Fix the bug. Run the test on the code with the fix. It must pass.
4. Run the other tests on both. They must keep their results.

Benchmarks for coding agents, such as SWE-bench, grade a fix in the same way. Elprovo runs these checks on copies that the author's machine never touched, twice on each commit, and alternates the commits to catch flaky results. The reviewer then reads one test and the line it failed with, and spends the rest of the review on the code.

## Try it

Elprovo needs Go 1.27, `git` and `/bin/sh`. The module is not published yet, so build it from a clone. Each run has an empty home directory, so give `go test` its caches through the environment:

```console
$ make build
$ export PATH="$PWD/bin:$PATH"
$ export GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)"
```

[`assets/demo-repo.sh`](assets/demo-repo.sh) makes a demo repository and prints its path. It holds a small shop API whose `SAVE10` coupon takes 10% off again on every use. Its branches follow the steps above:

```console
$ cd "$(sh assets/demo-repo.sh)"
$ git log --oneline --graph --all
* 07eb8ff (coupon-once) Apply a coupon once per cart
| * e0c6c74 (late-check) Apply a coupon once per cart, with the check after the discount
|/
* 236ae3a (HEAD -> test-only) Reproduce: SAVE10 takes 10% off again on every use
* fe71219 (main) Add the shop API
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
base fe71219  head 236ae3a  2 runs on the base commit
packages: ./cart

✓ reproduced      TestCouponAppliesOnce  cart/coupon_test.go
                  base: failed 2 of 2
                  base run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00

Tests: 1 reproduced.
Report: .elprovo/runs/7di1gy1e/report.json
```

`reproduced` means that the test failed in every run on `main`. Read the line under it: it must name the bug. A test that fails for another reason, such as a missing file, reproduces nothing. A test that does not build without the fix is `could not run`, and a test that passes is `not reproduced`.

Elprovo also prints a line for each run to standard error, such as `base run 1: ran (0.5s)`.

### 2. Fix it, and replay

The branch `coupon-once` adds the fix on top of the test:

```console
$ git switch -q coupon-once
$ elprovo replay --base main
base fe71219  head 07eb8ff  2 runs each
packages: ./cart

✓ fixed           TestCouponAppliesOnce  cart/coupon_test.go
                  base: failed 2 of 2   head: passed 2 of 2
                  base run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00

Changed tests: 1 fixed.
Other tests: 2 kept.
Report: .elprovo/runs/tojisl44/report.json
```

`fixed` means that the test failed on `main` and passed with the fix, in every run. Both copies ran the same test, so only the code made the difference. `2 kept` means that the other two tests in `./cart` passed on both commits, so the fix broke nothing there.

`elprovo replay --dry-run` lists the changed tests, and runs nothing:

```console
$ elprovo replay --base main --dry-run
base fe71219  head 07eb8ff  2 runs each (dry run)
packages: ./cart

changed  TestCouponAppliesOnce  cart/coupon_test.go

Dry run: no command ran.
```

### 3. A fix that does not fix

On the branch `late-check`, the fix checks the coupon only after it takes the discount. The test fails with the fix too:

```console
$ git switch -q late-check
$ elprovo replay --base main
base fe71219  head e0c6c74  2 runs each
packages: ./cart

✗ not fixed       TestCouponAppliesOnce  cart/coupon_test.go
                  base: failed 2 of 2   head: failed 2 of 2
                  head run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00
                  log: .elprovo/runs/e2hsazj0/head-1/test.log

Changed tests: 1 not fixed.
Other tests: 2 kept.
Report: .elprovo/runs/e2hsazj0/report.json
```

`elprovo replay` exits 1, and the output points to the log of the run that failed. `report.json` keeps every run. The changed test of this replay:

```json
{ "package": "./cart", "name": "TestCouponAppliesOnce", "file": "cart/coupon_test.go", "verdict": "not-fixed",
  "base": [{ "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00" }, { "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00" }],
  "head": [{ "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00" }, { "outcome": "failed", "message": "coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00" }] }
```

Add `.elprovo/` to your `.gitignore`.

## How a replay works

`elprovo replay` compares the base commit with the head commit:

- The **changed tests** are the `TestXxx` functions that the change adds, or edits in more than comments and spacing.
- The **packages** are the directories of the Go files that the change adds, modifies or deletes.
- The **other tests** are the tests in those packages that the change does not touch.

One run on a commit has four steps:

1. Elprovo writes the commit's code into a new directory, as a checkout writes it, and the head commit's test files beside it. So both commits run the same tests, and only the code differs.
2. It gives the run an empty home directory and an empty temporary directory.
3. It runs the test command, such as `go test -json`, on the packages, and reads the result of each test.
4. It deletes the directory.

It alternates the commits, two runs each by default: base 1, head 1, base 2, head 2. It never does two runs at the same time. `elprovo reproduce` does only the base runs, for the files that you name.

The base commit is the merge base with `origin/HEAD`, unless you give `--base`. Elprovo reads commits, not the working tree, so commit a test before you reproduce it. `elprovo replay -h` lists every option.

## Read the verdicts

In each run, each test passed, failed, was skipped, or is **absent**: the run printed no result for it, because its package did not build, its `TestMain` exited, or an earlier test panicked. Absent never counts as failed.

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
| `broke` | Passed without the change and failed with it |
| `already failing` | Failed on both commits |
| `not run` | Was absent on a commit, so Elprovo cannot compare it |
| `fixed` | Failed without the change and passed with it |
| `flaky`, `skipped` | As for a changed test |

`elprovo replay` exits 1 when a test is `not fixed`, `not reproduced`, `flaky` or `broke`, and 0 otherwise. With `--require`, it also exits 1 when no test is `fixed`, so use `--require` for a change that fixes a bug. It exits 2 when it could not replay, such as when the tests printed no results. `elprovo reproduce` exits 0 only when every test is `reproduced`, 2 when a test could not run, and 1 otherwise.

Programs read `report.json`, which [SPEC.md](SPEC.md) describes.

## elprovo.toml

The repository says once how to run its tests, in `elprovo.toml` at its root. It is [TOML](https://toml.io/en/v1.1.0). The demo's file:

```toml
version = 2
pass_env = ["GOCACHE", "GOMODCACHE"]

[tests]
files = ["**/*_test.go", "**/testdata/**"]
run = "go test -json -count=1 -timeout=5m {packages}"
report = "go-test-json"
```

| Field | Default | What it does |
| --- | --- | --- |
| `version` | | The format version. It must be `2`. |
| `tests.files` | | The files of the tests: the test files, and the files that they read. The base copy gets the head commit's version of these files. `**` matches any number of directories. |
| `tests.run` | | The test command. Elprovo puts the packages, such as `./cart`, where `{packages}` is. It must print `go test -json` output. |
| `tests.report` | | The format of that output. It must be `go-test-json`. |
| `tests.timeout` | `10m` | The longest time the test command may take. Give `go test` a shorter `-timeout`, so that a test that hangs fails with a message. |
| `env` | empty | A table of plain values that every command gets, such as `env.PORT = "8080"`. Not for secrets. |
| `pass_env` | empty | Names of variables that Elprovo copies from its own environment, such as `GOCACHE`. A listed variable that is not set is an error. |
| `[app]` | none | For tests that need a running app, such as a database: `start`, `ready` and `stop` commands, and `timeouts.start`, `timeouts.ready` and `timeouts.stop`. |

Put the files that the tests read, such as `testdata`, in `tests.files`. Otherwise a new test fails without the change only because its data file is missing, and that failure is not the bug.

Field names are case-sensitive, and a field that SPEC.md does not define is an error, so a misspelled field fails instead of being ignored. Elprovo's own [`elprovo.toml`](elprovo.toml) is like the demo's.

## In CI

The reviewer should trust a replay that their own CI ran, not a summary that the author pasted. This GitHub Actions job replays the tests of a pull request, puts the output in the job summary, and keeps the report and the logs:

```yaml
replay:
  if: github.event_name == 'pull_request'
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      with:
        fetch-depth: 0
        persist-credentials: false
    - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
      with:
        go-version-file: go.mod
    - name: Replay the tests of this pull request
      shell: bash
      env:
        BASE_REF: ${{ github.base_ref }}
      run: |
        set +e
        GOCACHE=$(go env GOCACHE)
        GOMODCACHE=$(go env GOMODCACHE)
        export GOCACHE GOMODCACHE
        elprovo replay --base "origin/$BASE_REF" > replay.txt
        status=$?
        cat replay.txt
        { echo '```'; cat replay.txt; echo '```'; } >> "$GITHUB_STEP_SUMMARY"
        exit "$status"
    - name: Keep the report and the logs
      if: always()
      uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
      with:
        name: elprovo-replay
        path: |
          .elprovo/runs/*/report.json
          .elprovo/runs/**/*.log
        if-no-files-found: ignore
```

The job needs `elprovo` on `PATH`. Make it a required status check of the branch, so that a pull request merges only when no test is `not fixed` or `broke`. On a private repository, GitHub offers required checks only on a paid plan.

Elprovo's own [pull request check](.github/workflows/elprovo.yml) does more. It builds `elprovo` from the base commit, so a pull request cannot change the tool that judges it. It also posts the result on the pull request, with a headline such as **Reproduced on main and fixed here. Nothing else broke.**

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
- If `docker` cannot reach its daemon in a run, set `DOCKER_HOST` to the daemon's socket and add `DOCKER_HOST` to `pass_env`.

`pass_env` can also name `HOME`. Do that only when a tool cannot work without your real home, because commands can then change files there.

## Security

A run is not a sandbox. Commands run as you, with your access to files, processes and the network, so a command can still reach your real home by its path. Elprovo runs the code of the change it replays: its tests, its test command and its `[app]` commands. Replay only changes whose code you would run yourself.

- In CI, replay pull requests from forks only in a job that has no secrets and a read-only token, such as a GitHub Actions job on `pull_request`.
- Variables in `pass_env` reach every command of every run, and the logs keep what the commands print. Pass test credentials only.

## What Elprovo does not do

- It does not write tests or fixes.
- It does not judge whether a test reproduces the right bug. Read the test, and the line that its base run failed with.
- It does not prove that a change is correct. It shows that a test fails without the change and passes with it, and that the other tests in the same packages kept their results. A weak test shows little, and tests in other packages do not run.
- It reads Go tests only, from one Go module at the repository root.
- It does not isolate a run beyond the environment, the home directory and the temporary directory. Docker images, caches, the network and anything outside the copy are shared.

## License

MIT. See [LICENSE](LICENSE).
