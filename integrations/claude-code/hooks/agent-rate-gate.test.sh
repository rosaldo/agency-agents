#!/usr/bin/env bash
# Canary for agent-rate-gate.sh: forged snapshots → expected decision.
# Usage: agent-rate-gate.test.sh [path-to-gate]   (default: the gate next to this file)
gate="${1:-$(dirname "$0")/agent-rate-gate.sh}"
dir=$(mktemp -d); trap 'rm -rf "$dir"' EXIT
now=$(date +%s); fail=0

snap() {  # snap <five_pct> <seven_pct> <observed_age_s> [reset_offset_s]
  local r=$(( now + ${4:-3600} ))
  printf '{"five_hour":{"used_percentage":%s,"resets_at":%s},"seven_day":{"used_percentage":%s,"resets_at":%s},"observed_at":%s}\n' \
    "$1" "$r" "$2" "$r" "$(( now - $3 ))" > "$dir/s.json"
}
check() {  # check <name> <expected: deny|pass|warn>
  local out got
  out=$(echo '{}' | AGENT_GATE_SNAPSHOT="$dir/s.json" "$gate")
  if   jq -e '.hookSpecificOutput.permissionDecision == "deny"' <<<"$out" >/dev/null 2>&1; then got=deny
  elif jq -e '.systemMessage | test("WITHOUT a quota check")' <<<"$out" >/dev/null 2>&1; then got=warn
  elif [[ -z "$out" ]]; then got=pass
  else got="?($out)"; fi
  if [[ "$got" == "$2" ]]; then echo "PASS  $1"; else echo "FAIL  $1: expected $2, got $got"; fail=1; fi
}

snap 81 10 5;        check "5h 81% denies"                 deny
snap 79 10 5;        check "5h 79% passes"                 pass
snap 80 90 5;        check "5h 80%/7d 90% passes (cap means 'above')" pass
snap 10 91 5;        check "7d 91% denies"                 deny
snap 99 99 700;      check "stale file passes with a warning"     warn
snap 99 99 5 -10;    check "expired window passes with a warning" warn
rm -f "$dir/s.json"; check "missing file passes with a warning"   warn
echo 'not-json' > "$dir/s.json"; check "invalid JSON passes with a warning" warn
exit $fail
