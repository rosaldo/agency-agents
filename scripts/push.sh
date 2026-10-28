#!/usr/bin/env bash
#
# push.sh — publishes the fork: builds every skill binary, pins the skills to a
# GitHub Release, pushes the branch and the tags, and publishes that Release.
# Run by the repository owner only. Binaries never go into git: they live in the
# Release, and git holds only each skill's release.conf (url) and SHA256SUMS.
#
# Usage: scripts/push.sh
#
# A skill ships binaries when it has skills/<skill>/release.conf. Its build script is
# scripts/build-<skill>.sh <out-dir>: it embeds $RELEASE_VERSION and writes the
# binaries plus SHA256SUMS into <out-dir> (here dist/<skill>/, gitignored).
#
# Order — every check runs before anything leaves this machine:
#   0. checks: on a branch, clean tree, tag v$VERSION exists, gh logged in
#   1. build every skill at the release version
#   2. pin: release.conf url + SHA256SUMS committed through commit.sh (a chore patch,
#      gate included); when already pinned to this version, gate only
#   3. git push of the branch, then of the tags
#   4. GitHub Release v<version> with every binary and one SHA256SUMS (create, or
#      re-upload with --clobber when it exists)
#
# The Release repository comes from $GH_REPO, else from the origin URL.
# Exit codes: 0 published · 1 refused or failed (the message says which step)
set -euo pipefail

ROOT="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
# shellcheck source=lib-version.sh
. "$ROOT/scripts/lib-version.sh"

log()  { printf '\n==> %s\n' "$*"; }
ok()   { printf '    ok  %s\n' "$*"; }
die()  { printf '\nerror: %s\n' "$*" >&2; exit 1; }
g()    { git -C "$ROOT" "$@"; }

case "${1:-}" in
  "") ;;
  -h | --help) sed -n '3,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
  *) die "unknown option '$1' (usage: scripts/push.sh)" ;;
esac

# --- 0. checks ---------------------------------------------------------------
BRANCH="$(g symbolic-ref -q --short HEAD)" || die "HEAD is detached — check out the branch to publish"
[[ -z "$(g status --porcelain)" ]] || die "uncommitted changes — close the version with scripts/commit.sh first"
ok "clean tree on $BRANCH"
VERSION="$(cat "$ROOT/VERSION" 2>/dev/null)" && [[ -n "$VERSION" ]] || die "VERSION missing at $ROOT"
g rev-parse -q --verify "refs/tags/v$VERSION" > /dev/null || die "tag v$VERSION does not exist — commit.sh creates it with the release commit"
ok "tag v$VERSION exists"

REPO="${GH_REPO:-$(g remote get-url origin | sed -E 's#^(git@github\.com:|https://github\.com/)##; s#\.git$##')}"
[[ "$REPO" =~ ^[^/]+/[^/]+$ ]] || die "cannot tell the GitHub repository from origin ($(g remote get-url origin)) — set GH_REPO=owner/name"
command -v gh > /dev/null || die "gh is not installed — https://cli.github.com"
gh auth status > /dev/null 2>&1 || die "gh is not logged in — run: gh auth login"
ok "gh logged in; Release repository $REPO"

SKILLS=()
for conf in "$ROOT"/skills/*/release.conf; do
  [[ -f "$conf" ]] || continue
  skill="$(basename "$(dirname "$conf")")"
  [[ -x "$ROOT/scripts/build-$skill.sh" ]] || die "skills/$skill/release.conf has no build script at scripts/build-$skill.sh"
  SKILLS+=("$skill")
done

# Already pinned to the current version (a re-run after a failed push): release it
# as is. Otherwise the pin commit is the next chore patch, so its version is known
# before building.
pinned_url() { sed -n 's/^url=//p' "$ROOT/skills/$1/release.conf"; }
TARGET="$(bump_version chore "$VERSION")"
if (( ${#SKILLS[@]} )) && [[ "$(pinned_url "${SKILLS[0]}")" == "https://github.com/$REPO/releases/download/v$VERSION" ]]; then
  TARGET="$VERSION"
fi
URL="https://github.com/$REPO/releases/download/v$TARGET"

# --- 1. build ------------------------------------------------------------------
ASSETS=()
for skill in "${SKILLS[@]}"; do
  log "build $skill (v$TARGET)"
  RELEASE_VERSION="v$TARGET" "$ROOT/scripts/build-$skill.sh" "$ROOT/dist/$skill" > /dev/null \
    || die "scripts/build-$skill.sh failed"
  [[ -s "$ROOT/dist/$skill/SHA256SUMS" ]] || die "scripts/build-$skill.sh wrote no SHA256SUMS"
  while read -r _ asset; do ASSETS+=("$ROOT/dist/$skill/${asset#\*}"); done < "$ROOT/dist/$skill/SHA256SUMS"
  ok "$(wc -l < "$ROOT/dist/$skill/SHA256SUMS" | tr -d ' ') binaries"
done

# --- 2. pin + gate ---------------------------------------------------------------
if [[ "$TARGET" == "$VERSION" ]]; then
  for skill in "${SKILLS[@]}"; do
    cmp -s "$ROOT/dist/$skill/SHA256SUMS" "$ROOT/skills/$skill/SHA256SUMS" \
      || die "rebuilt $skill binaries differ from the pinned skills/$skill/SHA256SUMS (another Go toolchain?) — rebuild with the toolchain that pinned v$VERSION"
  done
  log "already pinned to v$VERSION — gate only"
  "$ROOT/scripts/commit.sh" --gate || die "gate failed — nothing was pushed"
else
  for skill in "${SKILLS[@]}"; do
    cp "$ROOT/dist/$skill/SHA256SUMS" "$ROOT/skills/$skill/SHA256SUMS"
    sed -i.bak "s#^url=.*#url=$URL#" "$ROOT/skills/$skill/release.conf" && rm -f "$ROOT/skills/$skill/release.conf.bak"
    g add "skills/$skill/SHA256SUMS" "skills/$skill/release.conf"
  done
  if (( ${#SKILLS[@]} )); then
    log "pin ${SKILLS[*]} to the v$TARGET Release (commit.sh, gate included)"
    "$ROOT/scripts/commit.sh" chore "pin ${SKILLS[*]} binaries to the v$TARGET Release" \
      || { # the tree was clean at step 0, so this only undoes the pin
           g reset -q --hard; g clean -qf -- skills; die "pin commit failed (see above) — nothing was pushed"; }
    [[ "$(cat "$ROOT/VERSION")" == "$TARGET" ]] || die "commit.sh produced v$(cat "$ROOT/VERSION"), expected v$TARGET"
  else
    TARGET="$VERSION"
    "$ROOT/scripts/commit.sh" --gate || die "gate failed — nothing was pushed"
  fi
fi

# --- 3. git --------------------------------------------------------------------
log "git push origin $BRANCH"
g push -u origin "$BRANCH"
log "git push --tags"
g push origin --tags

# --- 4. Release ----------------------------------------------------------------
(( ${#SKILLS[@]} )) || { log "no skill ships binaries — v$TARGET pushed, no Release needed"; exit 0; }
SUMS="$ROOT/dist/SHA256SUMS"
for skill in "${SKILLS[@]}"; do cat "$ROOT/dist/$skill/SHA256SUMS"; done > "$SUMS"
if gh release view "v$TARGET" --repo "$REPO" > /dev/null 2>&1; then
  log "Release v$TARGET exists — re-uploading its assets"
  gh release upload "v$TARGET" "${ASSETS[@]}" "$SUMS" --repo "$REPO" --clobber
else
  log "Release v$TARGET"
  notes="$(grep -m1 "^- \*\*v$TARGET\*\*" "$ROOT/CHANGELOG.md" || echo "v$TARGET")"
  gh release create "v$TARGET" "${ASSETS[@]}" "$SUMS" --repo "$REPO" --verify-tag --title "v$TARGET" --notes "$notes"
fi
log "published v$TARGET: $URL"
