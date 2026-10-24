# Makes a repository whose change adds a YAML proof file, and checks that
# elprovo replay stops with status 2 and names the file.
set -eu
elprovo="$PWD/bin/elprovo"
repo="$TMPDIR/repo"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=proof GIT_AUTHOR_EMAIL=proof@example.invalid
export GIT_COMMITTER_NAME=proof GIT_COMMITTER_EMAIL=proof@example.invalid

git init -q -b main "$repo"
cd "$repo"
# Both files say the same, so that every version of Elprovo can read the
# repository.
printf 'version: 1\nstart: "true"\nready: "true"\n' > elprovo.yml
printf 'version = 1\nstart = "true"\nready = "true"\n' > elprovo.toml
echo off > feature.txt
git add -A && git commit -q -m base
base=$(git rev-parse HEAD)

mkdir proofs
printf 'claim: The feature is on.\nrun: grep -qx on feature.txt\n' > proofs/feature.yml
echo on > feature.txt
git add -A && git commit -q -m head

status=0
"$elprovo" replay --base "$base" > "$TMPDIR/out.txt" 2> "$TMPDIR/err.txt" || status=$?
cat "$TMPDIR/out.txt" "$TMPDIR/err.txt"
echo "elprovo replay exited with status $status"
[ "$status" -eq 2 ]
grep -q 'proofs/feature.yml: Elprovo reads proof files that end in .toml' "$TMPDIR/err.txt"
