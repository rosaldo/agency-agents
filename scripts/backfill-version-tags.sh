#!/usr/bin/env bash
#
# backfill-version-tags.sh — versions the owner's commits that predate commit.sh,
# without rewriting history: one LOCAL annotated tag per commit, and VERSION set to
# the last one. Run it once, before the first commit.sh release. It never pushes.
#
# The first owner commit is v1.0.0; every later commit by the same author bumps by
# the commit.sh rule (lib-version.sh), read from its Conventional Commits type.
# Commits by anyone else (upstream) are skipped and get no tag. Re-running is safe:
# a tag that already sits on the same commit is kept.
#
# Usage: scripts/backfill-version-tags.sh <first-owner-commit> [author]
#   author  git --author pattern; default: git config user.email
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
# shellcheck source=lib-version.sh
. "$REPO_DIR/scripts/lib-version.sh"
fail() { echo "error: $*" >&2; exit 1; }

[[ $# -ge 1 ]] || { echo "Usage: $0 <first-owner-commit> [author]" >&2; exit 2; }
first="$(git -C "$REPO_DIR" rev-parse -q --verify "$1^{commit}")" || fail "'$1' is not a commit in this repository"
author="${2:-$(git -C "$REPO_DIR" config user.email)}"
[[ -n "$author" ]] || fail "no author given and git config user.email is empty — pass the author as the 2nd argument"

version="" created=0 kept=0
while IFS=$'\t' read -r sha subj; do
  if [[ -z "$version" ]]; then
    [[ "$sha" == "$first" ]] || continue
    version=1.0.0
  else
    [[ ! "$subj" =~ ^[a-z]+\(v[0-9]+\. ]] || fail "$sha is already a commit.sh release ($subj) — backfill is only for history before commit.sh"
    type="$(commit_type "$subj")" && version="$(bump_version "$type" "$version")" \
      || fail "no known Conventional Commits type in $sha: '$subj' — known: feat fix refactor docs chore test build perf security debug major"
  fi
  if existing="$(git -C "$REPO_DIR" rev-parse -q --verify "refs/tags/v$version^{commit}")"; then
    [[ "$existing" == "$sha" ]] || fail "tag v$version already points at $existing, not $sha — delete it (git tag -d v$version) and re-run"
    kept=$((kept + 1))
  else
    git -C "$REPO_DIR" tag -a "v$version" "$sha" -m "Release v$version"
    created=$((created + 1))
  fi
done < <(git -C "$REPO_DIR" log --reverse --author="$author" --format=$'%H\t%s')

[[ -n "$version" ]] || fail "$first is not a commit by '$author' on this branch"
echo "$version" > "$REPO_DIR/VERSION"
echo "Tags created: $created, already there: $kept. VERSION = $version (not pushed)."
