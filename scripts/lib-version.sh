#!/usr/bin/env bash
#
# lib-version.sh — the fork's version rule, shared by commit.sh and
# backfill-version-tags.sh so a backfilled version and a committed one can
# never disagree. Sourced, not executed. Bash 3.2+.

# bump_version <type> <X.Y.Z> — prints the next version; fails on an unknown type.
#   patch: fix security debug docs chore test build perf
#   minor: feat refactor
#   major: major
bump_version() {
  local major minor patch
  [[ "$2" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r major minor patch <<< "$2"
  case "$1" in
    fix | security | debug | docs | chore | test | build | perf) patch=$((patch + 1)) ;;
    feat | refactor) minor=$((minor + 1)); patch=0 ;;
    major) major=$((major + 1)); minor=0; patch=0 ;;
    *) return 1 ;;
  esac
  echo "${major}.${minor}.${patch}"
}

# subject_prefix <type> — the word that opens the release subject. security and
# debug are fixes, and say so.
subject_prefix() {
  case "$1" in
    security | debug) echo fix ;;
    *) echo "$1" ;;
  esac
}

# commit_type <subject> — the Conventional Commits type of a subject
# ("feat(scope): x" -> feat). A breaking "type!:" is not matched on purpose:
# this rule has an explicit "major" type, and a guessed major is worse than a refusal.
commit_type() {
  [[ "$1" =~ ^([a-z]+)[\(:] ]] || return 1
  echo "${BASH_REMATCH[1]}"
}
