---
name: elprovo-reproduce
description: Fix a bug reproduce-first with Elprovo. Write a Go test that fails because of the bug, commit it, check with elprovo reproduce that it fails on the base commit for the bug's reason, make the smallest fix, and show with elprovo replay --require that the test is fixed and the other tests are kept. Use when you fix a bug in a Go repository that has elprovo.toml, when the user asks to reproduce a bug, prove a fix, or run elprovo reproduce or elprovo replay. Not for new features without a bug, or for changes to documentation or formatting.
---

# Reproduce, fix, prove

A reviewer trusts what a machine re-ran, not your report. A fix is shown when a test fails without it and passes with it. `elprovo reproduce` shows the first half before you fix anything, and `elprovo replay` shows both halves after. Elprovo's SPEC.md has the full rules. This page has what you need.

## 1. Check the setup

The repository root needs `elprovo.toml` of version 2. If it has none, write one, and tell the user that it needs their review, because every replay runs it:

```toml
version = 2
pass_env = ["GOCACHE", "GOMODCACHE"]

[tests]
files = ["**/*_test.go", "**/testdata/**"]
run = "go test -json -count=1 -timeout=5m {packages}"
report = "go-test-json"
```

Set the caches before you run Elprovo: `export GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)"`. Each run gets an empty `HOME` and `TMPDIR`, and only `PATH`, `USER` and `LANG` from the environment. A test that needs more, such as a database, needs an `[app]` table with `start`, `ready` and `stop`, as SPEC.md section 1 shows.

## 2. Reproduce the bug as a test

Write one Go test that fails because of the bug, in the package where the bug is. Make it a top-level function, `func TestXxx(t *testing.T)`. Elprovo gives verdicts to top-level test functions only. A subtest, or a method of a testify suite, counts only through its top-level function, so even in a repository that uses suites, put the reproduction in its own function. A good test:

- checks the exact result that the bug gets wrong, such as the value, the text or the status;
- fails with a message that names the bug, such as `total after SAVE10 twice is 81.00; want 90.00`;
- uses only code that exists before the fix, so that it builds without the fix;
- keeps its data files in a path that `tests.files` matches, such as `testdata/`;
- creates its own data, and does not depend on other tests or on their order;
- never reads Git or the commit to decide its result.

Commit the test alone, before any fix. Commit only if you are allowed to. If you are not, tell the user that the test is ready to reproduce after they commit it.

## 3. Check that it reproduces

Run `elprovo reproduce --base <base> <test file>`, where the base is usually the merge base with the default branch, such as `origin/main`.

- `reproduced`: the test failed in every run. Read the line `base run 1: …`. It must name the bug. If it names something else, such as a missing file, change the test.
- `not reproduced`: the test passed without the fix. It does not show the bug. Change the test until it fails for the bug's reason.
- `could not run`: the test did not build without the fix, or its package failed. Make the test use only code that the base commit has.
- `flaky`: the test gave different results. Remove the cause, such as time, randomness or order.

Go on only when every test is `reproduced`.

## 4. Fix the bug

Make the smallest change that fixes the bug. Do not change the test to make it pass. If the test turns out to be wrong, go back to step 2, and say so in your report.

Commit the fix.

## 5. Prove the fix

Run `elprovo replay --base <base> --require`.

- The test must be `fixed`: it failed without the fix and passed with it.
- The other tests in the packages must be `kept`. `broke` means that your fix broke a test: fix the code. `not run` means that a package did not build on one commit: read its line.
- `not fixed`, `not reproduced` and `flaky` mean the fix is not done.

If `elprovo` is not installed and you may not install it, do not run the steps by hand and do not call the bug fixed. Tell the user that the test and the fix are ready, and that they need `elprovo`.

## 6. Report

If the repository replays pull requests in CI, the reviewer trusts that check, not your report. Name the test, so that the reviewer reads it first, and point to the check. If the repository has no such check, paste the summary of your local `elprovo replay` and say that it ran on your machine. Give the verdict, the runs and the base line, such as "fixed: base failed 2 of 2, head passed 2 of 2; base run 1: coupon_test.go:12: total after SAVE10 twice is 81.00; want 90.00", and the count of kept tests.

Do not call a bug fixed when no test is `fixed`.
