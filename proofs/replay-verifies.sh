# Makes a repository with two commits, whose proof fails on the first and
# passes on the second, and checks that elprovo replay calls it verified.
set -eu
elprovo="$PWD/bin/elprovo"
repo="$TMPDIR/repo"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=proof GIT_AUTHOR_EMAIL=proof@example.invalid
export GIT_COMMITTER_NAME=proof GIT_COMMITTER_EMAIL=proof@example.invalid

git init -q -b main "$repo"
cd "$repo"
echo off > feature.txt
git add -A && git commit -q -m base
base=$(git rev-parse HEAD)

mkdir proofs
printf 'version: 1\nstart: "true"\nready: "true"\n' > elprovo.yml
printf 'claim: The feature is on.\nrun: grep -qx on feature.txt\n' > proofs/feature.yml
echo on > feature.txt
git add -A && git commit -q -m head

"$elprovo" replay --base "$base" > "$TMPDIR/out.txt" 2> "$TMPDIR/err.txt"
cat "$TMPDIR/out.txt"
grep -qx '✓ verified  proofs/feature.yml' "$TMPDIR/out.txt"
grep -qx '1 verified.' "$TMPDIR/out.txt"
