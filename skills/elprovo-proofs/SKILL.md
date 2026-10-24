---
name: elprovo-proofs
description: Prove each behavior a code change claims with an Elprovo proof, a command that fails without the change and passes with it, and replay the proofs before reporting the work as done. Use when you add or fix behavior and are about to say it works, when a repository has elprovo.yml or a proofs/ directory, or when the user asks to prove a change, add proofs, or run elprovo replay. Not for changes with no behavior claim, such as documentation or formatting.
---

# Prove the change

A reviewer trusts what a machine re-ran, not your report. For every behavior you claim, add a proof: a command that exits 0 when the claim holds. Elprovo runs it on fresh copies of the code without your change and with it. The full format is in Elprovo's SPEC.md. This page has what you need.

## 1. List the claims

Write each behavior you will claim as one plain sentence, at most 200 characters, that a reviewer can check. For example: "A coupon applies once per order, even from two browser tabs." Claim only what you will prove. A change that claims no behavior needs no proof. Say so in your report.

## 2. Make sure the app can start

The repository root needs `elprovo.yml`. If it has none, write one:

```yaml
version: 1
start: docker compose up --detach --build      # must exit 0, with the app running
ready: curl -fsS http://localhost:8080/healthz  # exits 0 when the app takes requests
stop: docker compose down --volumes            # removes everything start made
```

- `start` must work from the tracked files alone, with no `.env` file, local data, or cache that only your machine has.
- `ready` must check that the app answers, not only that a process exists.
- `stop` must remove containers, volumes and processes, so the next run starts clean.
- Every replay runs this file, so tell the user that a new or changed `elprovo.yml` needs their review.

## 3. Write one proof per claim

Put each proof in `proofs/<name>.yml`, where `<name>` uses lowercase letters, digits and dashes:

```yaml
claim: A coupon applies once per order, even from two browser tabs.
run: npx playwright test proofs/coupon-once.spec.ts --trace on --output "$ELPROVO_ARTIFACTS"
```

Keep the test and every other file the proof needs in `proofs/`. A good proof:

- tests from the outside: the user interface with Playwright, the HTTP API with requests, or the command line. Use a unit test only when the claim is about a library function;
- checks the exact result the claim states, such as the value, the text or the status. It does not stop at "the page loads";
- exits with a status other than 0 when the claim is false;
- creates its own data, and does not depend on another proof or on the order of proofs;
- never reads the source code, Git, or the environment to decide its result;
- writes traces and screenshots to `$ELPROVO_ARTIFACTS`.

Write the proof before the fix when you can, and watch it fail on the current code. If the claim also held before your change, as in a refactor, add `before: skip` and say why in your report.

Never weaken a proof to make it pass. When a proof fails, fix the code, or remove the claim from your report.

## 4. Replay

Replay reads commits, so the change must be committed. Commit only if you are allowed to. If you are not, tell the user that the proofs are ready to replay after they commit.

If `elprovo` is installed, run `elprovo replay --base <base>`. The base is usually the merge base with the default branch.

If it is not installed, replay by hand. Do the runs in this order: base run 1, head run 1, base run 2, head run 2. Skip the base runs when `before: skip`. Make one folder for all copies first: `copies=$(mktemp -d)`. For one run of a proof on commit `C`, with head commit `H`, both written as full commit IDs:

1. Make a fresh copy of `C` without its proofs: `d="$copies/elprovo-$(openssl rand -hex 4)"; mkdir -p "$d" && git archive C -- . ':(exclude)proofs' | tar -x -C "$d"`
2. Add the head's proofs: `git archive H proofs | tar -x -C "$d"`
3. If `$d/elprovo.yml` does not exist, run `git show "H:elprovo.yml" > "$d/elprovo.yml"`. In zsh, write a variable there as `"${H}:elprovo.yml"`, because zsh reads `$H:e` as a modifier.
4. In `$d`, run each command with only these variables: `env -i PATH="$PATH" HOME="$HOME" USER="$USER" LANG="$LANG" TMPDIR="$TMPDIR" ELPROVO_RUN_ID=<id> ELPROVO_ARTIFACTS=<absolute path outside $d> <the env and pass_env values> sh -c '<command>'`
5. Run `start`. Then run `ready` once a second until it exits 0. Then run the proof's `run`. Exit 0 is a pass, and any other exit is a fail. If `start` or `ready` fails, the run is "not run".
6. Always run `stop`. Leave the copy, and name the `$copies` folder in your report, so the user can delete it.

Give each proof one result:
- **verified:** every head run passed, and every base run failed, or the base runs were skipped.
- **failed:** every head run failed.
- **flaky:** the runs disagree.
- **no-effect:** every run passed on both commits.
- **not-run:** a run did not run.

## 5. Report

List each claim with its result and its runs, such as "verified: base failed 2 of 2, head passed 2 of 2". Write "manual replay" when you replayed by hand. Name the proof files, so the reviewer reads them first. A claim that is not verified is not done. Say which claims failed and why. Do not claim a behavior that no proof shows.
