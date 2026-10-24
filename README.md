# Elprovo

Elprovo checks the claims in a pull request by running them. Each claim comes with a proof: a command that exits 0 when the claim holds. Elprovo runs every proof on a fresh copy of the code, without the change and with it, and reports which claims it verified.

**Status:** design. This repository holds the interface and no code yet. [SPEC.md](SPEC.md) defines the files, the replay and the results. The first release of the command line tool will be 0.1.

## Why

Coding agents report "done, all tests pass", and it is not always true. To find out, a reviewer reads the whole diff, or checks out the branch and tries it by hand.

Elprovo asks for something a machine can check instead. For each behavior that a change claims, the change adds a short proof. Elprovo runs the proof itself, on a copy of the code that the author's machine never touched. The reviewer reads a few short proofs and their results, not the whole diff.

By default, a proof counts only when it fails without the change and passes with it. A proof that passes either way shows nothing about the change.

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

Then:

```console
$ elprovo replay
base 3f2a1c0  head 9b7e4d2  1 proof, 2 runs each

✓ verified  proofs/coupon-once.yml
            A coupon applies once per order, even from two browser tabs.
            base: failed 2 of 2   head: passed 2 of 2

1 of 1 claims verified.
```

When the fix is incomplete, the same proof fails on the head commit, and the output points to the log and the Playwright trace of the failed run:

```console
✗ failed    proofs/coupon-once.yml
            A coupon applies once per order, even from two browser tabs.
            head: failed 2 of 2
            log: .elprovo/runs/k3v9q2m7/coupon-once/head-1/proof.log
```

## How a replay works

`elprovo replay` takes each proof that the pull request adds or changes. For each one:

1. It copies the base commit into a new directory, puts the pull request's `proofs/` directory in it, starts the app, and runs the proof. Then it stops the app and deletes the directory.
2. It does the same for the head commit.
3. It repeats both steps, two times by default. Every run starts from a new copy.

Elprovo replays commits, not the working tree. Commit a change before you replay it.

## Results

| Result | Meaning |
| --- | --- |
| `verified` | Failed on the base commit and passed on the head commit, in every run |
| `failed` | Failed on the head commit in every run |
| `flaky` | Passed in some runs and failed in others |
| `no-effect` | Passed on the base commit too, so it doesn't show what the change did |
| `not-run` | The app didn't start, so the claim wasn't tested |

`elprovo replay` exits 0 only when every proof is verified. [SPEC.md](SPEC.md) gives the exit status in full.

## Writing proofs

- Write one claim per proof file, as one plain sentence that a reviewer can check against the proof.
- Test the behavior from the outside where you can: through the app's user interface, its API or its command line.
- Keep everything a proof needs in `proofs/`.
- Don't make a proof depend on another proof, on the order of proofs, or on the commit it runs against.
- A claim that a change keeps something working, such as a refactor, can't fail on the base commit. Set `before: skip` in its proof file, and Elprovo runs it on the head commit only.

## For coding agents

Add this to the repository's `AGENTS.md`:

> When a change adds or fixes behavior, add one proof for each claim in `proofs/`, as SPEC.md in the Elprovo repository describes. Run `elprovo replay` and put its summary in your report. Don't claim a behavior that no proof shows.

## What Elprovo does not do

- It doesn't read or score the code.
- It doesn't write proofs.
- It doesn't prove that a change is correct. It proves the claims that its proofs make. A weak proof shows little, so a reviewer still reads the proofs.
- Version 0.x replays on the current machine, in a fresh copy of each commit. It doesn't isolate a run from the machine's Docker images, caches or network.
