#!/usr/bin/env bash
#
# test-versioning.sh — regression tests for the fork's release tooling:
# scripts/commit.sh, scripts/gen-changelog.sh and scripts/backfill-version-tags.sh.
#
# Every case runs in a throwaway git repository with a bare "origin" next to it, so
# nothing here touches this repository's history, tags or network. The bare origin is
# how "commit.sh never pushes" is checked: it must still be empty at the end.
#
# Usage: ./scripts/test-versioning.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
failed=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; failed=1; fi; }

T="$WORK/repo"
git init -q "$T"
git init -q --bare "$WORK/origin.git"
git -C "$T" remote add origin "$WORK/origin.git"
git -C "$T" config user.name Owner
git -C "$T" config user.email owner@example.com
git -C "$T" config commit.gpgsign false
git -C "$T" config tag.gpgsign false
mkdir -p "$T/scripts"
for s in commit.sh gen-changelog.sh backfill-version-tags.sh lib-version.sh; do
  cp "$REPO_ROOT/scripts/$s" "$T/scripts/$s" 2>/dev/null
done
git -C "$T" add scripts 2>/dev/null

# commit_as <author> <subject> — one commit touching its own file.
n=0
commit_as() {
  n=$((n + 1)); echo "$n" > "$T/f$n"
  git -C "$T" add "f$n"
  git -C "$T" -c user.name="$1" -c user.email="$1@example.com" commit -qm "$2"
}
tags() { git -C "$T" tag -l 'v*' | sort -V | tr '\n' ' '; }
tag_at() { git -C "$T" rev-parse -q --verify "refs/tags/$1^{commit}"; }

echo "backfill-version-tags.sh"
commit_as upstream "feat: something from upstream"
commit_as owner "feat(a): first owner commit"; first="$(git -C "$T" rev-parse HEAD)"
commit_as upstream "fix: upstream fix in between"; upstream_sha="$(git -C "$T" rev-parse HEAD)"
commit_as owner "fix(b): a fix"
commit_as owner "perf(c): faster"
commit_as owner "feat(d): a feature"
commit_as owner "build(e): a build change"; last="$(git -C "$T" rev-parse HEAD)"

"$T/scripts/backfill-version-tags.sh" "$first" owner@example.com > "$WORK/out" 2>&1
check "backfill succeeds" '[[ $? -eq 0 ]]'
check "first owner commit is v1.0.0, then the commit.sh rule" '[[ "$(tags)" == "v1.0.0 v1.0.1 v1.0.2 v1.1.0 v1.1.1 " ]]'
check "v1.0.0 is the first owner commit" '[[ "$(tag_at v1.0.0)" == "$first" ]]'
check "v1.1.1 is the last owner commit" '[[ "$(tag_at v1.1.1)" == "$last" ]]'
check "upstream commits get no tag" '[[ -z "$(git -C "$T" tag --points-at "$upstream_sha")" ]]'
check "tags are annotated" '[[ "$(git -C "$T" cat-file -t v1.0.0)" == tag ]]'
check "VERSION ends at the last owner version" '[[ "$(cat "$T/VERSION" 2>/dev/null)" == 1.1.1 ]]'
"$T/scripts/backfill-version-tags.sh" "$first" owner@example.com > "$WORK/out" 2>&1
check "backfill is idempotent" '[[ $? -eq 0 && "$(tags)" == "v1.0.0 v1.0.1 v1.0.2 v1.1.0 v1.1.1 " ]]'
"$T/scripts/backfill-version-tags.sh" "$upstream_sha" owner@example.com > "$WORK/out" 2>&1
check "backfill refuses a first commit that is not the owner's" '[[ $? -ne 0 ]]'

echo "gen-changelog.sh"
"$T/scripts/gen-changelog.sh" > /dev/null 2>&1
check "gen-changelog succeeds" '[[ $? -eq 0 ]]'
CL="$T/CHANGELOG.md"
check "says it is generated" 'grep -q "do not edit by hand" "$CL" 2>/dev/null'
check "one entry per tagged owner commit" '[[ $(grep -c "^- " "$CL" 2>/dev/null) -eq 5 ]]'
check "newest first" '[[ "$(grep -m1 "^- " "$CL" 2>/dev/null)" == "- **v1.1.1** ("*") · \`build\` — e: a build change" ]]'
check "upstream commits stay out" '! grep -q upstream "$CL" 2>/dev/null'

echo "commit.sh"
git -C "$T" add VERSION CHANGELOG.md 2>/dev/null; git -C "$T" commit -qm "chore: baseline" 2>/dev/null
before="$(git -C "$T" rev-parse HEAD)"
echo x > "$T/g1"; git -C "$T" add g1
"$T/scripts/commit.sh" feat "add a widget" > "$WORK/out" 2>&1
check "feat commits" '[[ $? -eq 0 ]]'
subject="$(git -C "$T" log -1 --format=%s)"
check "feat bumps minor and puts the version in the subject" '[[ "$subject" == "feat(v1.2.0): add a widget" ]]'
check "VERSION is 1.2.0 in the commit" '[[ "$(git -C "$T" show HEAD:VERSION 2>/dev/null)" == 1.2.0 ]]'
check "annotated tag v1.2.0 on HEAD" '[[ "$(tag_at v1.2.0)" == "$(git -C "$T" rev-parse HEAD)" && "$(git -C "$T" cat-file -t v1.2.0)" == tag ]]'
check "CHANGELOG in the same commit lists v1.2.0" 'git -C "$T" show HEAD:CHANGELOG.md 2>/dev/null | grep -q "^- \*\*v1.2.0\*\*.*add a widget"'
check "one commit, not two" '[[ "$(git -C "$T" rev-parse HEAD~1)" == "$before" ]]'
check "no AI trailer in the message" '! git -C "$T" log -1 --format=%B | grep -qiE "co-authored-by|generated with"'

bump() { echo "$1" > "$T/g-$1"; git -C "$T" add "g-$1"; "$T/scripts/commit.sh" "$1" "$1 change" > "$WORK/out" 2>&1; git -C "$T" log -1 --format=%s; }
check "fix bumps patch" '[[ "$(bump fix)" == "fix(v1.2.1): fix change" ]]'
check "security is a patch under the fix prefix" '[[ "$(bump security)" == "fix(v1.2.2): security change" ]]'
check "docs bumps patch" '[[ "$(bump docs)" == "docs(v1.2.3): docs change" ]]'
check "perf bumps patch" '[[ "$(bump perf)" == "perf(v1.2.4): perf change" ]]'
check "build bumps patch" '[[ "$(bump build)" == "build(v1.2.5): build change" ]]'
check "refactor bumps minor" '[[ "$(bump refactor)" == "refactor(v1.3.0): refactor change" ]]'
check "major bumps major" '[[ "$(bump major)" == "major(v2.0.0): major change" ]]'

head_before="$(git -C "$T" rev-parse HEAD)"
"$T/scripts/commit.sh" nope "x" > "$WORK/out" 2>&1
check "unknown type is refused" '[[ $? -ne 0 && "$(git -C "$T" rev-parse HEAD)" == "$head_before" ]]'
"$T/scripts/commit.sh" fix > "$WORK/out" 2>&1
check "missing message is refused" '[[ $? -ne 0 && "$(git -C "$T" rev-parse HEAD)" == "$head_before" ]]'

printf 'if then\n' > "$T/broken.sh"; git -C "$T" add broken.sh
"$T/scripts/commit.sh" fix "broken shell" > "$WORK/out" 2>&1
check "gate failure blocks the commit" '[[ $? -ne 0 && "$(git -C "$T" rev-parse HEAD)" == "$head_before" ]]'
check "gate failure leaves VERSION and tags alone" '[[ "$(cat "$T/VERSION")" == 2.0.0 && -z "$(git -C "$T" tag -l v2.0.1)" ]]'

git -C "$T" rm -q --cached broken.sh; rm "$T/broken.sh"
if command -v go > /dev/null; then
  # A committed engine with a data race: --gate stages nothing, so this only
  # fails if --gate tests the engine anyway and with -race.
  E="$T/quant-engine"; mkdir -p "$E"
  printf 'module racy\n\ngo 1.21\n' > "$E/go.mod"
  printf 'package racy\n\nimport "testing"\n\nfunc TestRace(t *testing.T) {\n\tx, done := 0, make(chan bool)\n\tgo func() { x++; done <- true }()\n\tx++\n\t<-done\n\t_ = x\n}\n' > "$E/racy_test.go"
  git -C "$T" add quant-engine; git -C "$T" commit -qm "chore: racy engine"
  "$T/scripts/commit.sh" --gate > "$WORK/out" 2>&1
  check "--gate tests the engine with -race even with nothing staged" '[[ $? -ne 0 ]] && grep -q "DATA RACE" "$WORK/out"'
else
  echo "  skip --gate engine check (go not installed)"
fi

check "nothing was pushed" '[[ -z "$(git -C "$WORK/origin.git" for-each-ref)" ]]'

if (( failed )); then echo "test-versioning: FAILED"; exit 1; fi
echo "test-versioning: all passed"
