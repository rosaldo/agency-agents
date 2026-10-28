#!/usr/bin/env bash
#
# test-push.sh — regression tests for scripts/push.sh, with no network and no real push.
#
# A throwaway repository gets a fake skill (release.conf + a fake build script), a
# bare local "origin", and a fake `gh` on PATH that only records what it was asked to
# do. The checks: what lands in origin, what the Release would carry, and that every
# refusal happens before anything is pushed or released.
#
# Usage: ./scripts/test-push.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
failed=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; failed=1; fi; }

T="$WORK/repo" O="$WORK/origin.git" GHLOG="$WORK/gh.log"
git init -q -b work "$T"
git init -q --bare "$O"
git -C "$T" remote add origin "$O"
git -C "$T" config user.name Owner
git -C "$T" config user.email owner@example.com
git -C "$T" config commit.gpgsign false
git -C "$T" config tag.gpgsign false
mkdir -p "$T/scripts" "$T/skills/demo" "$WORK/bin"
for s in commit.sh gen-changelog.sh lib-version.sh push.sh; do
  cp "$REPO_ROOT/scripts/$s" "$T/scripts/$s" 2>/dev/null
done
printf 'dist/\n' > "$T/.gitignore"
printf '# demo\nbinary=demo\nurl=\n' > "$T/skills/demo/release.conf"
cat > "$T/scripts/build-demo.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$1"
printf '%s\n' "$RELEASE_VERSION" > "$1/demo-linux-amd64"
(cd "$1" && sha256sum demo-linux-amd64 > SHA256SUMS)
EOF
chmod +x "$T/scripts/build-demo.sh"
echo 1.0.0 > "$T/VERSION"
git -C "$T" add -A
git -C "$T" commit -qm "feat(v1.0.0): start"
git -C "$T" tag -a v1.0.0 -m "Release v1.0.0"

# Fake gh: records every call; `release view` succeeds once a release was created.
cat > "$WORK/bin/gh" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$GHLOG"
case "\$1 \$2" in
  "auth status") [[ -z "\${FAKE_GH_UNAUTH:-}" ]] ;;
  "release view") [[ -f "$WORK/released-\$3" ]] ;;
  "release create") touch "$WORK/released-\$3" ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH" GH_REPO=owner/demo
URL=https://github.com/owner/demo/releases/download

push() { : > "$GHLOG"; "$T/scripts/push.sh" > "$WORK/out" 2>&1; local r=$?; [[ -n "${DEBUG:-}" ]] && cat "$WORK/out"; return $r; }
origin_has() { git -C "$O" rev-parse -q --verify "$1" > /dev/null; }

echo "push.sh refusals"
echo dirty > "$T/dirty.txt"
push
check "dirty tree is refused" '[[ $? -ne 0 ]]'
check "dirty tree: nothing pushed, no release" '[[ -z "$(git -C "$O" for-each-ref)" ]] && ! grep -q "^release" "$GHLOG"'
rm "$T/dirty.txt"

FAKE_GH_UNAUTH=1 push
check "gh not logged in is refused before any push" '[[ $? -ne 0 && -z "$(git -C "$O" for-each-ref)" ]]'

printf 'if then\n' > "$T/broken.sh"; git -C "$T" add broken.sh; git -C "$T" commit -qm "chore: broken"
push
check "gate failure is refused" '[[ $? -ne 0 ]]'
check "gate failure: nothing pushed, no release" '[[ -z "$(git -C "$O" for-each-ref)" ]] && ! grep -q "^release" "$GHLOG"'
git -C "$T" rm -q broken.sh; git -C "$T" commit -qm "chore: unbroken"

echo "push.sh first publish"
push
check "push succeeds" '[[ $? -eq 0 ]]'
check "pins via commit.sh as a patch release" '[[ "$(git -C "$T" log -1 --format=%s)" == chore\(v1.0.1\):* && "$(cat "$T/VERSION")" == 1.0.1 ]]'
check "release.conf url points at the v1.0.1 Release" 'grep -qx "url=$URL/v1.0.1" "$T/skills/demo/release.conf"'
check "SHA256SUMS next to release.conf matches the build" 'cmp -s "$T/skills/demo/SHA256SUMS" "$T/dist/demo/SHA256SUMS"'
check "binary carries the release version" '[[ "$(cat "$T/dist/demo/demo-linux-amd64")" == v1.0.1 ]]'
check "binaries stay out of git" '[[ -z "$(git -C "$T" ls-files dist)" ]]'
check "branch pushed at HEAD" '[[ "$(git -C "$O" rev-parse refs/heads/work 2>/dev/null)" == "$(git -C "$T" rev-parse HEAD)" ]]'
check "tags pushed" 'origin_has refs/tags/v1.0.1 && origin_has refs/tags/v1.0.0'
check "Release v1.0.1 created with the binary and SHA256SUMS" 'grep -E "^release create v1.0.1 .*demo-linux-amd64.*SHA256SUMS" "$GHLOG" | grep -q -- "--repo owner/demo"'

echo "push.sh re-run of the same version"
head_before="$(git -C "$T" rev-parse HEAD)"
push
check "re-run succeeds" '[[ $? -eq 0 ]]'
check "re-run makes no new commit" '[[ "$(git -C "$T" rev-parse HEAD)" == "$head_before" ]]'
check "re-run re-uploads to the existing Release" 'grep -q "^release upload v1.0.1 .*--clobber" "$GHLOG" && ! grep -q "^release create" "$GHLOG"'

if (( failed )); then echo "test-push: FAILED"; exit 1; fi
echo "test-push: all passed"
