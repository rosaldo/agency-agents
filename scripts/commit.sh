#!/usr/bin/env bash
#
# commit.sh — the only way to commit on the fork's branch: runs the gate, bumps
# VERSION by the commit type, commits with the version in the subject, regenerates
# CHANGELOG.md into that same commit, and tags it. It never pushes.
#
# Usage: scripts/commit.sh <type> "<message>"     (stage your files first)
#        scripts/commit.sh --gate                   only run the gate (push.sh uses it)
#   fix | security | debug       patch   subject: fix(vX.Y.Z): message
#   docs | chore | test          patch   subject: <type>(vX.Y.Z): message
#   build | perf                 patch
#   feat | refactor              minor
#   major                        major
#
# Exit codes: 0 committed and tagged · 1 gate or git failure · 2 usage
# Emergency only: AGENCY_SKIP_GATE=1 scripts/commit.sh ...
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
VERSION_FILE="$REPO_DIR/VERSION"
# shellcheck source=lib-version.sh
. "$REPO_DIR/scripts/lib-version.sh"

fail() { echo "error: $*" >&2; exit 1; }
usage() {
  echo "Usage: $0 <fix|security|debug|docs|chore|test|build|perf|feat|refactor|major> \"<message>\"  |  $0 --gate" >&2
  exit 2
}

# run_check <script> — runs a repo check quietly; on failure shows its output and stops.
# A check that does not exist is skipped: the gate runs what the repo has.
run_check() {
  local script="$REPO_DIR/scripts/$1" out
  [[ -x "$script" ]] || return 0
  echo "-> gate: $1"
  out="$("$script" 2>&1)" || { echo "$out"; fail "$1 failed — fix it, then commit again"; }
}

# The gate runs BEFORE the bump: a broken commit never becomes a version, and a
# version never becomes a tag. test-convert-outputs.sh is left out on purpose: its
# manifest drift is advisory (CONTRIBUTING.md, "Before Submitting").
gate() {
  if [[ "${AGENCY_SKIP_GATE:-}" == 1 ]]; then
    echo "warning: gate skipped (AGENCY_SKIP_GATE=1)" >&2
    return
  fi

  echo "-> gate: bash -n"
  local sh
  while IFS= read -r sh; do
    [[ -f "$REPO_DIR/$sh" ]] || continue
    bash -n "$REPO_DIR/$sh" || fail "syntax error in $sh"
  done < <(git -C "$REPO_DIR" ls-files -co --exclude-standard '*.sh')

  run_check lint-agents.sh
  run_check check-divisions.sh
  run_check test-install.sh
  run_check test-versioning.sh
  run_check test-push.sh

  # The engine is checked when the commit touches it.
  local engine=quant-engine
  if git -C "$REPO_DIR" diff --cached --name-only | grep -q "^$engine/"; then
    command -v go > /dev/null || fail "this commit touches $engine but go is not installed — install Go to run its tests"
    echo "-> gate: go vet + go test ($engine)"
    (cd "$REPO_DIR/$engine" && go vet ./... && go test -count=1 ./...) || fail "$engine: go vet or go test failed"
  fi

  echo "gate ok"
}

release() {
  local type="$1" msg="$2" current new subject
  [[ -f "$VERSION_FILE" ]] || fail "VERSION not found at $VERSION_FILE — on a fresh history run scripts/backfill-version-tags.sh first"
  current="$(cat "$VERSION_FILE")"
  new="$(bump_version "$type" "$current")" || fail "VERSION holds '$current', expected X.Y.Z"
  git -C "$REPO_DIR" rev-parse -q --verify "refs/tags/v$new" > /dev/null \
    && fail "tag v$new already exists — VERSION ($current) is behind the tags; set it to the latest tag's version"

  echo "$new" > "$VERSION_FILE"
  subject="$(subject_prefix "$type")(v${new}): ${msg}"
  git -C "$REPO_DIR" add "$VERSION_FILE"
  git -C "$REPO_DIR" commit -q -m "$subject"

  # CHANGELOG goes into the SAME commit, before the tag, so the tag points at a
  # commit that already contains its own entry.
  "$REPO_DIR/scripts/gen-changelog.sh" > /dev/null
  git -C "$REPO_DIR" add CHANGELOG.md
  git -C "$REPO_DIR" commit -q --amend --no-edit

  git -C "$REPO_DIR" tag -a "v$new" HEAD -m "Release v$new"
  echo "Committed: $subject"
  echo "Tagged:    v$new (local; nothing pushed)"
}

if [[ "${1:-}" == --gate && $# -eq 1 ]]; then gate; exit 0; fi
[[ $# -eq 2 && -n "$2" ]] || usage
case "$1" in
  fix | security | debug | docs | chore | test | build | perf | feat | refactor | major) ;;
  *) echo "error: unknown type '$1'" >&2; usage ;;
esac

gate
release "$1" "$2"
