---
name: elprovo-proofs
description: Prove each behavior a code change claims with an Elprovo proof, a command that fails without the change and passes with it, and replay the proofs with elprovo replay before reporting the work as done. Use when you add or fix behavior and are about to say it works, when a repository has elprovo.yml or a proofs/ directory, or when the user asks to prove a change, add proofs, or run elprovo replay. Not for changes with no behavior claim, such as documentation or formatting.
---

# Prove the change

A reviewer trusts what a machine re-ran, not your report. For every behavior you claim, add a proof: a command that exits 0 when the claim holds. `elprovo replay` runs it on fresh copies of the code without your change and with it. Elprovo's SPEC.md has the full format. This page has what you need.

## 1. List the claims

Write each behavior you will claim as one plain sentence, at most 200 characters, that a reviewer can check. For example: "A coupon applies once per order, even from two browser tabs." Claim only what you will prove. A change that claims no behavior needs no proof. Say so in your report.

## 2. Make sure the app can start

The repository root needs `elprovo.yml`. If it has none, write one. For an app that keeps running:

```yaml
version: 1
start: docker compose up --detach --build      # must exit 0, with the app running
ready: curl -fsS http://localhost:8080/healthz  # exits 0 when the app takes requests
stop: docker compose down --volumes            # removes everything start made
```

For a command line tool, `start` only builds it, and `stop` is not needed:

```yaml
version: 1
start: go build -o bin/tool .
ready: ./bin/tool --help
```

- `start` must work from the tracked files alone, with no `.env` file, local data or cache that only your machine has.
- Each run gets an empty `HOME` and `TMPDIR`, and only `PATH`, `USER` and `LANG` from the environment. A tool that needs more, such as `DOCKER_HOST` or a test credential, must be named in `pass_env`.
- `ready` must check that the app answers, not only that a process exists.
- `stop` must remove what `start` left running.
- Every replay runs this file, so tell the user that a new or changed `elprovo.yml` needs their review.

## 3. Write one proof per claim

Put each proof in `proofs/<name>.yml`, where `<name>` uses lowercase letters, digits and dashes:

```yaml
claim: A coupon applies once per order, even from two browser tabs.
run: npx playwright test proofs/coupon-once.spec.ts --trace on --output "$ELPROVO_ARTIFACTS"
```

The proof runs in the root of the copy. Keep the test and every other file the proof needs in `proofs/`. Only `.yml` files directly in `proofs/` are proofs. A good proof:

- tests from the outside: the user interface with Playwright, the HTTP API with requests, or the command line. Use a unit test only when the claim is about a library function;
- checks the exact result the claim states, such as the value, the text or the status. It does not stop at "the page loads";
- exits with a status other than 0 when the claim is false;
- creates its own data, including any files under `HOME`, and does not depend on another proof or on the order of proofs;
- never reads the source code, Git or the commit to decide its result;
- writes traces and screenshots to `$ELPROVO_ARTIFACTS`.

Write the proof before the fix when you can, and watch it fail on the current code.

If the claim also held before your change, as in a refactor, add `before: skip`. The result is then `held`, which never shows that the proof can fail. So for such a proof, break the code on purpose once, check that the proof fails, undo the break, and say in your report that you did.

Never weaken a proof to make it pass. When a proof fails, fix the code, or remove the claim from your report.

## 4. Replay

Replay reads commits, so the change must be committed. Commit only if you are allowed to. If you are not, tell the user that the proofs are ready to replay after they commit.

Run `elprovo replay --base <base>`, where the base is usually the merge base with the default branch. `elprovo replay --dry-run` checks the files first without running anything.

If `elprovo` is not installed and you may not install it, do not replay the proofs by hand and do not call any claim verified. Tell the user the proofs are ready and that they need `elprovo replay`.

## 5. Report

Paste the summary of `elprovo replay`: each claim with its result and its runs, such as "verified: base failed 2 of 2, head passed 2 of 2". Name the proof files, so the reviewer reads them first.

- `verified` is the only result that shows what the change did.
- `held` shows the claim holds now. Say how you checked that the proof can fail.
- `failed`, `flaky`, `no-effect` and `not-run` mean the claim is not done. Say which claims and why.

Do not claim a behavior that no proof shows.
