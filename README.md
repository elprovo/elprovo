# Elprovo

Elprovo checks the claims in a pull request by running them. Each claim comes with a proof: a command that exits 0 when the claim holds. Elprovo runs every proof on a fresh copy of the code, without the change and with it, and reports which claims it verified.

**Status:** 0.1, not released yet. It runs on macOS and Linux. [SPEC.md](SPEC.md) defines the files, the replay and the results, and [CHANGELOG.md](CHANGELOG.md) lists what each version changed.

## Why

Coding agents report "done, all tests pass", and it is not always true. To find out, a reviewer reads the whole diff, or checks out the branch and tries it by hand.

Elprovo asks for something a machine can check instead. For each behavior that a change claims, the change adds a short proof. Elprovo runs the proof itself, on a copy of the code that the author's machine never touched. The reviewer reads a few short proofs and their results, not the whole diff.

By default, a proof counts only when it fails without the change and passes with it. A proof that passes either way shows nothing about the change.

## Install

Elprovo needs Go 1.27, `git` and `/bin/sh`. The module is not published yet, so build it from a clone:

```console
$ make build
$ ./bin/elprovo version
```

## Example

A repository says once how to start its app, in `elprovo.yml`:

```yaml
version: 1
start: docker compose up --detach --build
ready: curl -fsS http://localhost:8080/healthz
stop: docker compose down --volumes
```

A pull request that fixes a coupon bug in a shop app adds one proof for its claim, `proofs/coupon-once.yml`:

```yaml
claim: A coupon applies once per order, even from two browser tabs.
run: npx playwright test proofs/coupon-once.spec.ts --trace on --output "$ELPROVO_ARTIFACTS"
```

The proof's test is an ordinary Playwright test, `proofs/coupon-once.spec.ts`. The two tabs share one browser context, so they share one cart:

```ts
import { test, expect } from '@playwright/test';

test('a coupon applies once, even from two tabs', async ({ browser }) => {
  const context = await browser.newContext();
  const tabs = [await context.newPage(), await context.newPage()];
  for (const tab of tabs) await tab.goto('http://localhost:8080/cart');

  await Promise.all(tabs.map((tab) => tab.getByRole('button', { name: 'Apply SAVE10' }).click()));

  await tabs[0].reload();
  await expect(tabs[0].getByTestId('discount')).toHaveText('-10.00');
});
```

Then, after the change is committed:

```console
$ elprovo replay
base 3f2a1c0  head 9b7e4d2  1 proof, 2 runs each

✓ verified  proofs/coupon-once.yml
            A coupon applies once per order, even from two browser tabs.
            base: failed 2 of 2   head: passed 2 of 2

1 verified.
Report: .elprovo/runs/k3v9q2m7/report.json
```

Elprovo also prints a line for each run to standard error, so you can follow a long replay.

When the fix is incomplete, the same proof fails on the head commit, and the output points to the log of the failed run. The Playwright trace is in that run's `artifacts` directory:

```console
✗ failed    proofs/coupon-once.yml
            A coupon applies once per order, even from two browser tabs.
            base: failed 2 of 2   head: failed 2 of 2
            log: .elprovo/runs/k3v9q2m7/coupon-once/head-1/proof.log
```

A command line tool has nothing to keep running, so `start` only builds it, and `elprovo.yml` needs no `stop`:

```yaml
version: 1
start: go build -o bin/lnk .
ready: ./bin/lnk --help
```

## How a replay works

`elprovo replay` takes each proof that the change adds or modifies. For each one:

1. It copies the base commit into a new directory, puts the change's `proofs/` directory in it, starts the app, and runs the proof. Then it stops the app and deletes the directory.
2. It does the same for the head commit.
3. It repeats both steps, two times by default. Every run starts from a new copy.

The base commit is the merge base with `origin/HEAD`, unless you give `--base`. Elprovo replays commits, not the working tree, so commit a change before you replay it.

`elprovo replay --all` also replays the proofs that the change does not touch, on the head commit only. That shows their claims still hold. `elprovo replay --dry-run` checks the files and lists the proofs without running anything. `elprovo replay -h` lists every option.

## Results

| Result | Meaning |
| --- | --- |
| `verified` | Failed on the base commit and passed on the head commit, in every run |
| `held` | Passed on the head commit in every run, and was not compared with the base commit |
| `failed` | Failed on the head commit in every run |
| `flaky` | Passed in some runs and failed in others |
| `no-effect` | Passed on the base commit too, so it doesn't show what the change did |
| `not-run` | The app didn't start or didn't get ready, so the claim wasn't tested |

`elprovo replay` exits 0 when every proof is `verified` or `held`, 1 when a proof is `failed`, `flaky` or `no-effect`, and 2 when a proof is `not-run` or Elprovo could not replay. The full rules are in [SPEC.md](SPEC.md). Programs read `report.json` in the replay's directory.

Add `.elprovo/` to your `.gitignore`.

## What a run can see

Each command runs in the copy, with `/bin/sh -c`, and gets only these environment variables:

- `PATH`, `USER` and `LANG` from your environment;
- `HOME` and `TMPDIR`, set to empty directories that belong to the run;
- the values in `env`, and the variables you name in `pass_env`;
- `ELPROVO_RUN_ID` and `ELPROVO_ARTIFACTS`, an empty directory for traces and screenshots. After the run, Elprovo moves what is there to the run's directory in `.elprovo`.

So tools that read or write the home directory use the run's own, not yours. This is not a sandbox: commands run as you, so a command can still reach your real home by its path, and anything else you can reach. Two things follow from the empty home directory:

- Tool managers such as mise and asdf find tools through the home directory, so their shims can fail in a run. Put the real tool directory first in `PATH`, or name what the tool needs in `pass_env`.
- If `docker` cannot reach its daemon in a run, set `DOCKER_HOST` to the daemon's socket and add `DOCKER_HOST` to `pass_env`.

`pass_env` can also name `HOME`. Do that only when a tool cannot work without your real home, because commands can then change files there.

## Security

Elprovo runs the commands in the change it replays: `start`, `ready`, `stop` and every proof. Replay only changes whose code you would run yourself.

- In CI, replay pull requests from forks only in a job that has no secrets and a read-only token, such as a GitHub Actions job on `pull_request`.
- Variables in `pass_env` reach every command of every run, and the logs keep what the commands print. Pass test credentials only.

## In CI

This GitHub Actions job replays the proofs of a pull request and puts the output in the job summary:

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
```

The job needs `elprovo` on `PATH`, and the tools that `elprovo.yml` uses. Elprovo's own [workflow](.github/workflows/ci.yml) builds it first.

## Writing proofs

- Write one claim per proof file, as one plain sentence that a reviewer can check against the proof.
- Test the behavior from the outside where you can: through the app's user interface, its API or its command line.
- Keep everything a proof needs in `proofs/`. Only `.yml` files directly in `proofs/` are proofs. Every other file there is a helper file.
- Don't make a proof depend on another proof, on the order of proofs, or on the commit it runs against.
- A claim that a change keeps something working, such as a refactor, can't fail on the base commit. Set `before: skip` in its proof file. Its result is then `held`, which doesn't show that the proof can fail, so read such a proof with care.

## For coding agents

Add this to the repository's `AGENTS.md`:

> When a change adds or fixes behavior, add one proof for each claim in `proofs/`, as SPEC.md in the Elprovo repository describes. Run `elprovo replay` and put its summary in your report. Don't claim a behavior that no proof shows.

For agents that load skills, such as Claude Code, [skills/elprovo-proofs/SKILL.md](skills/elprovo-proofs/SKILL.md) gives the same rule with the steps.

## What Elprovo does not do

- It doesn't read or score the code.
- It doesn't write proofs.
- It doesn't prove that a change is correct. It proves the claims that its proofs make. A weak proof shows little, so a reviewer still reads the proofs.
- It doesn't isolate a run from the machine beyond the environment, the home directory and the temporary directory. Docker images, caches, the network and anything outside the copy are shared.

## License

MIT. See [LICENSE](LICENSE).
