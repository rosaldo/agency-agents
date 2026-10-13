#!/usr/bin/env bash
# PreToolUse gate for the Agent tool: denies a subagent dispatch when the 5h window
# is above 80% or the 7d window is above 90%. Source: the snapshot written by the
# status line (~/.claude/state/rate-limits.json, from the official rate_limits.* fields).
# Missing, unreadable, stale (>10 min) or expired-window data does NOT block, it only
# warns, so a broken snapshot never stalls work. Agents already in flight are unaffected.
# Disable: AGENT_GATE_OFF=1 in claude's environment, or remove the "Agent" matcher block
# from ~/.claude/settings.json. Test: ~/.claude/hooks/agent-rate-gate.test.sh
cat >/dev/null  # the hook input is not used; drain stdin
[[ -n "$AGENT_GATE_OFF" ]] && exit 0

snap="${AGENT_GATE_SNAPSHOT:-$HOME/.claude/state/rate-limits.json}"
warn() {
  jq -nc --arg m "Quota gate: $1 — dispatch allowed WITHOUT a quota check." \
    '{hookSpecificOutput:{hookEventName:"PreToolUse", additionalContext:$m}, systemMessage:$m}'
}
[[ -r "$snap" ]] || { warn "snapshot $snap missing or unreadable"; exit 0; }

jq -ec --argjson now "$(date +%s)" --argjson m5 80 --argjson m7 90 --argjson stale 600 '
  def short: if . == "five_hour" then "5h" else "7d" end;
  def win($k; $max): .[$k] as $w
    | if ($w | type) != "object" or ($w.used_percentage | type) != "number" or (($w.resets_at // 0) <= $now)
      then {k: $k, state: "absent"}
      elif $w.used_percentage > $max
      then {k: $k, state: "over", pct: $w.used_percentage, max: $max, reset: $w.resets_at}
      else {k: $k, state: "ok"} end;
  def out($deny; $m):
    if $deny
    then {hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $m}, systemMessage: $m}
    else {hookSpecificOutput: {hookEventName: "PreToolUse", additionalContext: $m}, systemMessage: $m} end;
  if ($now - (.observed_at // 0)) > $stale then
    out(false; "Quota gate: snapshot older than 10 min (no data) — dispatch allowed WITHOUT a quota check.")
  else
    [win("five_hour"; $m5), win("seven_day"; $m7)] as $ws
    | ($ws | map(select(.state == "over"))) as $over
    | if ($over | length) > 0 then
        out(true; "Subagent dispatch DENIED by the quota gate: "
          + ($over | map("\(.k | short) window at \(.pct)% (cap \(.max)%), resets \(.reset | strflocaltime("%a %d/%m %H:%M"))") | join("; "))
          + ". Agents in flight continue; save the partial work to the ledger and wait for the reset.")
      elif ($ws | any(.state == "absent")) then
        out(false; "Quota gate: " + ($ws | map(select(.state == "absent") | .k | short) | join(" and "))
          + " window missing or expired in the snapshot (no data) — dispatch allowed WITHOUT a quota check for it.")
      else empty end
  end' "$snap" 2>/dev/null
rc=$?
# rc 4 = jq produced no output (under the cap); any other rc = invalid JSON or a gate bug → warn, never block
(( rc == 0 || rc == 4 )) || warn "snapshot $snap unreadable (invalid JSON or a bug in the gate itself)"
exit 0
