# Elprovo

Elprovo checks the claims of a pull request by running them. Each claim comes with a proof: a command that exits 0 when the claim holds. Elprovo runs the proof on a fresh copy of the code without the change, and on a fresh copy with it. A claim counts only when its proof fails without the change and passes with it.

![elprovo replay verifies the claim of a fix: its proof fails on the base commit and passes on the head commit](assets/demo.gif)

**Status:** 0.2.0. It runs on macOS and Linux. It does not support Windows, because it runs commands with `/bin/sh` in Unix process groups. [SPEC.md](SPEC.md) is the contract: the files, the replay, the results and `report.json`. [CHANGELOG.md](CHANGELOG.md) lists what each version changed.

## Why

A pull request says "fixed, and the tests pass". That sentence costs nothing to write, whether a person or a coding agent writes it. Passing tests show that the code runs. They do not show that the change did what it claims: a test can pass with the change and without it, or not touch the claim at all.

The check that does show it is a counterfactual. Run the same check on the code without the change and on the code with it. If it fails first and then passes, the change made the claim true. Elprovo runs that check for each claim, on copies that the author's machine never touched, and repeats it to catch flaky results. The reviewer then reads a few claims, their proofs and their results, and spends the rest of the review on the parts of the change that no claim covers.

## Try it

Elprovo needs Go 1.27, `git` and `/bin/sh`. The module is not published yet, so build it from a clone:

```console
$ make build
$ export PATH="$PWD/bin:$PATH"
```

[`assets/demo-repo.sh`](assets/demo-repo.sh) makes a demo repository and prints its path. It holds a small shop API whose `SAVE10` coupon takes 10% off again each time the shop gets it, and a change on the branch `coupon-once` that fixes the bug. The demo needs `curl`, and port 8080 must be free.

```console
$ cd "$(sh assets/demo-repo.sh)"
$ git log --oneline
7e13753 (HEAD -> coupon-once) Apply a coupon once per cart
65041a7 (main) Add the shop API
```

The repository says once how to start its app, in `elprovo.toml`:

```toml
version = 1
start = "go build -o bin/shop . && { ./bin/shop & echo $! > shop.pid; }"
ready = "curl -fsS http://localhost:8080/healthz"
stop = "kill $(cat shop.pid)"
```

The fix adds one proof for its claim, `proofs/coupon-once.toml`:

```toml
claim = "A coupon applies once per cart, even when the shop gets it twice."
run = '''
set -eu
curl -fsS -X POST 'http://localhost:8080/cart/items?price=100.00'
curl -fsS -X POST 'http://localhost:8080/cart/coupon?code=SAVE10'
curl -fsS -X POST 'http://localhost:8080/cart/coupon?code=SAVE10'
total=$(curl -fsS http://localhost:8080/cart/total)
[ "$total" = 90.00 ] || { echo "FAIL: the total is $total after SAVE10 twice, not 90.00"; exit 1; }
'''
```

Replay it against `main`:

```console
$ elprovo replay --base main
base 65041a7  head 7e13753  1 proof, 2 runs each

✓ verified  proofs/coupon-once.toml
            A coupon applies once per cart, even when the shop gets it twice.
            base: failed 2 of 2   head: passed 2 of 2
            base run 1 ended with: FAIL: the total is 81.00 after SAVE10 twice, not 90.00

1 verified.
Report: .elprovo/runs/opq01jji/report.json
```

Elprovo also prints a line for each run to standard error, such as `proofs/coupon-once.toml  base run 1: fail (5.5s)`, so you can follow a long replay. `elprovo replay --dry-run` checks the files and lists the proofs, and runs nothing:

```console
$ elprovo replay --base main --dry-run
base 65041a7  head 7e13753  1 proof, 2 runs each (dry run)

proofs/coupon-once.toml  base and head
            A coupon applies once per cart, even when the shop gets it twice.

Dry run: no command ran.
```

The demo has two more branches, `late-check` and `outside-test`. The next sections show what they teach.

## How a replay works

`elprovo replay` selects the proofs that the change adds or modifies. One run of a proof on a commit has four steps:

1. Elprovo writes the files of the commit into a new directory, as a checkout writes them. It leaves out `proofs/` and writes the head commit's `proofs/` in its place, so both commits run the same proof.
2. It gives the run an empty home directory and an empty temporary directory.
3. It runs `start`, waits until `ready` exits 0, runs the proof, and runs `stop`.
4. It deletes the directory.

For each proof, it alternates the commits, two runs each by default: base 1, head 1, base 2, head 2. It never does two runs at the same time, so two apps never compete for a port.

The base commit is the merge base with `origin/HEAD`, unless you give `--base`. Elprovo reads commits, not the working tree, so commit a change before you replay it. `elprovo replay --all` also replays the proofs that the change does not touch, on the head commit only, to show that their claims still hold. `elprovo replay -h` lists every option.

## Read the result

| Result | What the runs did |
| --- | --- |
| `verified` | Failed on the base commit and passed on the head commit, in every run |
| `held` | Passed on the head commit in every run, and did not run on the base commit |
| `failed` | Failed on the head commit in every run |
| `flaky` | Passed in some runs and failed in others |
| `no-effect` | Passed on the base commit too, so it shows nothing about the change |
| `not-run` | The app did not start or did not get ready, so the claim was not tested |

`elprovo replay` exits 0 when every proof is `verified` or `held`, 1 when a proof is `failed`, `flaky` or `no-effect`, and 2 when a proof is `not-run` or Elprovo could not replay. Programs read `report.json`, which [SPEC.md](SPEC.md) describes.

`verified` is the only result that shows what the change did. Under it, Elprovo shows the last line that the first base run printed. Read that line, because a run fails on any exit status other than 0, for any reason. On the branch `outside-test`, the fix is the same, but the proof runs a test that the change adds outside `proofs/`:

```console
$ git switch -q outside-test
$ elprovo replay --base main
base 65041a7  head 9d0fc02  1 proof, 2 runs each

✓ verified  proofs/coupon-once.toml
            A coupon applies once per cart, even when the shop gets it twice.
            base: failed 2 of 2   head: passed 2 of 2
            base run 1 ended with: sh: tests/coupon-once.sh: No such file or directory

1 verified.
Report: .elprovo/runs/shiocgsa/report.json
```

The base copy does not have `tests/coupon-once.sh`, so the base runs failed before they tested anything. The result is `verified`, but it shows nothing about the fix. That is why everything a proof needs belongs in `proofs/`. A proof that prints why it fails, as the demo's proof does, makes the line easy to check.

On the branch `late-check`, the fix checks the coupon only after it takes the discount, so the proof fails on the head commit too. The output points to the log of the failed run, and the run's `artifacts` directory holds what the proof wrote to `$ELPROVO_ARTIFACTS`:

```console
$ git switch -q late-check
$ elprovo replay --base main
base 65041a7  head 0e3476e  1 proof, 2 runs each

✗ failed    proofs/coupon-once.toml
            A coupon applies once per cart, even when the shop gets it twice.
            base: failed 2 of 2   head: failed 2 of 2
            head run 1 ended with: FAIL: the total is 81.00 after SAVE10 twice, not 90.00
            log: .elprovo/runs/k25zpmnj/coupon-once/head-1/proof.log

1 failed.
Report: .elprovo/runs/k25zpmnj/report.json
```

`report.json` keeps every run. Base run 1 of this replay, with the same line in `last_line`:

```json
{ "run": 1, "outcome": "fail", "exit_code": 1, "timed_out": false, "seconds": 5.5, "reason": "", "last_line": "FAIL: the total is 81.00 after SAVE10 twice, not 90.00", "artifacts_dropped": false, "dir": "coupon-once/base-1" }
```

Add `.elprovo/` to your `.gitignore`.

## The files

Both files are [TOML](https://toml.io/en/v1.1.0). TOML quotes every string, so a claim or a command is exactly what the file shows. A multi-line literal string, `'''…'''`, keeps a script as written. Field names are case-sensitive, and a field that SPEC.md does not define is an error, so a misspelled field fails instead of being ignored.

`elprovo.toml`, at the repository root:

| Field | Default | What it does |
| --- | --- | --- |
| `version` | | The format version. It must be `1`. |
| `start` | | Starts the app, or only builds it when nothing has to keep running. It must exit 0. |
| `ready` | | Exits 0 when the app takes requests. Elprovo runs it once a second until it does. |
| `stop` | none | Stops what `start` left running. It runs after every run, also after a failure. |
| `env` | empty | A table of plain values that every command gets, such as `env.PORT = "8080"`. Not for secrets. |
| `pass_env` | empty | Names of variables that Elprovo copies from its own environment, such as `["DOCKER_HOST"]`. |
| `timeouts.start`, `timeouts.ready`, `timeouts.stop` | `10m`, `5m`, `2m` | The longest time each step may take. |

A command line tool has nothing to keep running, so `start` only builds it, and `stop` is not needed. Elprovo's own [`elprovo.toml`](elprovo.toml) is one:

```toml
version = 1
start = "go build -o bin/elprovo ./cmd/elprovo"
ready = "./bin/elprovo version"
```

A proof file is `proofs/<name>.toml`, where `<name>` uses lowercase letters, digits and dashes:

| Field | Default | What it does |
| --- | --- | --- |
| `claim` | | The claim, as one sentence of at most 200 characters. |
| `run` | | The command. It exits 0 when the claim holds. |
| `before` | `"fail"` | `"skip"` for a claim that also held before the change, such as a refactor that keeps a behavior. The proof then runs on the head commit only, so its best result is `held`. |
| `timeout` | `10m` | The longest time `run` may take. |

Every other file in `proofs/` is a helper file that proofs can use, such as a test, a fixture or a shared script. A `.yml` or `.yaml` file directly in `proofs/` is an error, so put a YAML helper file in a subdirectory.

## Write good proofs

- Write one claim per proof, as one plain sentence that a reviewer can check against the proof.
- Test from the outside where you can: through the app's user interface, its API or its command line.
- Check the exact result that the claim states, such as the value, the text or the status.
- Print why the proof fails, as its last line.
- Keep everything the proof needs in `proofs/`.
- Make each proof create its own data. Don't make it depend on another proof, on the order of proofs, or on the commit it runs against.
- A `held` proof never shows that it can fail. For such a proof, break the code on purpose once, see the proof fail, and undo the break.

## In CI

The reviewer should trust a replay that their own CI ran, not a summary that the author pasted. This GitHub Actions job replays the proofs of a pull request, puts the output in the job summary, and keeps the report and the logs:

```yaml
replay:
  if: github.event_name == 'pull_request'
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      with:
        fetch-depth: 0
        persist-credentials: false
    - name: Replay the proofs of this pull request
      shell: bash
      env:
        BASE_REF: ${{ github.base_ref }}
      run: |
        set +e
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

The job needs `elprovo` on `PATH`, and the tools that `elprovo.toml` uses. Elprovo's own [workflow](.github/workflows/ci.yml) builds it first.

Make the job a required status check of the branch, so that a pull request merges only when its proofs hold. On a private repository, GitHub offers required checks only on a paid plan.

## For coding agents

Add this to the repository's `AGENTS.md`:

> When a change adds or fixes behavior, add one proof for each claim in `proofs/`, as SPEC.md in the Elprovo repository describes. Run `elprovo replay`, and check that each base run ended with the claim failing, not with a missing file or command. In your report, name the proof files and point to the CI check that replays them. If there is no such check, paste the local summary and say that it ran locally. Don't claim a behavior that no proof shows.

For agents that load skills, such as Claude Code, [skills/elprovo-proofs/SKILL.md](skills/elprovo-proofs/SKILL.md) gives the same rule with the steps.

## What a run can see

Each command runs in the copy, with `/bin/sh -c`, and gets only these environment variables:

- `PATH`, `USER` and `LANG` from your environment;
- `HOME` and `TMPDIR`, set to empty directories that belong to the run;
- the values in `env`, and the variables that `pass_env` names;
- `ELPROVO_RUN_ID`, and `ELPROVO_ARTIFACTS`, an empty directory for traces and screenshots. After the run, Elprovo moves what is there to the run's directory in `.elprovo`.

So tools that read or write the home directory use the run's own, not yours. No variable says which commit runs. Three things follow from the empty home directory:

- Tools keep caches in the home directory, so each run starts with empty caches. In the demo, a run takes about 5.5 seconds, most of it to build the shop with an empty cache. To reuse Go's build cache, run `export GOCACHE="$(go env GOCACHE)"` and add `GOCACHE` to `pass_env`. The head runs of the demo then take less than 2 seconds.
- Tool managers such as mise and asdf find tools through the home directory, so their shims can fail in a run. Put the real tool directory first in `PATH`, or name what the tool needs in `pass_env`.
- If `docker` cannot reach its daemon in a run, set `DOCKER_HOST` to the daemon's socket and add `DOCKER_HOST` to `pass_env`.

`pass_env` can also name `HOME`. Do that only when a tool cannot work without your real home, because commands can then change files there.

## Security

A run is not a sandbox. Commands run as you, with your access to files, processes and the network, so a command can still reach your real home by its path. Elprovo runs the commands of the change it replays: `start`, `ready`, `stop` and every proof. Replay only changes whose code you would run yourself.

- In CI, replay pull requests from forks only in a job that has no secrets and a read-only token, such as a GitHub Actions job on `pull_request`.
- Variables in `pass_env` reach every command of every run, and the logs keep what the commands print. Pass test credentials only.

## What Elprovo does not do

- It does not judge whether the claims are the right ones, or read the code. The reviewer does that.
- It does not write proofs.
- It does not prove that a change is correct. It shows that each proof fails without the change and passes with it. A weak proof shows little.
- The last line of a run is what the proof printed, so a proof can print a false reason. Read the proof too.
- It does not isolate a run beyond the environment, the home directory and the temporary directory. Docker images, caches, the network and anything outside the copy are shared.

## License

MIT. See [LICENSE](LICENSE).
