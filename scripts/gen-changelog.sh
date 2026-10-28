#!/usr/bin/env bash
#
# gen-changelog.sh — writes CHANGELOG.md from git. Do not edit CHANGELOG.md by hand.
#
# An entry is a commit that carries a version, read from one of two places:
#   1. its subject, "type(vX.Y.Z): message" — every commit made by commit.sh;
#   2. an annotated v* tag on it — the owner's commits from before commit.sh,
#      versioned by backfill-version-tags.sh without rewriting history.
# Commits with neither (everything merged from upstream) are not part of the fork's
# releases and stay out. Newest first.
#
# commit.sh runs this BEFORE it creates the tag, so a release's own entry has to come
# from its subject, not from its tag.
set -euo pipefail

ROOT="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
OUT="$ROOT/CHANGELOG.md"

{
  echo "# Changelog"
  echo
  echo "> Generated from git by \`scripts/gen-changelog.sh\` — do not edit by hand."
  echo
  awk -F'\t' '
    NR == FNR { if ($1 != "") tag[$1] = $2; next }   # annotated tag -> its commit
    {
      sha = $1; date = $2; subj = $3
      if (match(subj, /^(feat|fix|refactor|docs|chore|test|major|build|perf)\(v[0-9]+\.[0-9]+\.[0-9]+\): /)) {
        type = substr(subj, 1, index(subj, "(") - 1)
        ver = substr(subj, index(subj, "(") + 1); ver = substr(ver, 1, index(ver, ")") - 1)
        msg = substr(subj, RLENGTH + 1)
      } else if (sha in tag) {
        ver = tag[sha]; type = "-"; msg = subj
        if (match(subj, /^[a-z]+(\([^)]*\))?: /)) {
          head = substr(subj, 1, RLENGTH - 2); msg = substr(subj, RLENGTH + 1)
          type = head; scope = ""
          if (index(head, "(")) { type = substr(head, 1, index(head, "(") - 1); scope = substr(head, index(head, "(") + 1); sub(/\)$/, "", scope) }
          if (scope != "") msg = scope ": " msg
        }
      } else next
      printf "- **%s** (%s) · `%s` — %s\n", ver, date, type, msg
    }
  ' <(git -C "$ROOT" for-each-ref --format='%(*objectname)%09%(refname:short)' 'refs/tags/v*') \
    <(git -C "$ROOT" log --pretty=$'%H\t%cs\t%s')
} > "$OUT"

echo "CHANGELOG.md generated: $(grep -c '^- ' "$OUT") entries."
