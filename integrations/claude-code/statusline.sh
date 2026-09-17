#!/usr/bin/env bash
# Agency status line for Claude Code.
#   [Opus 5] ⎇ main | ████░░░░░░ 42% ctx | [5h] 34% ↻14:30 | [7d] 12% ↻mon13:00
# Context bar turns yellow at 50% and red at 75% — the cue to start a fresh
# session and run /maestro (the ledger brings the pipeline back). The 5h / 7d
# figures are the subscription rate limits; absent on API-key billing.
# Installed by scripts/install.sh --tool claude-code; fields documented at
# https://code.claude.com/docs/en/statusline
input=$(cat)
model=$(jq -r '.model.display_name // "?"' <<<"$input")
dir=$(jq -r '.workspace.current_dir // ""' <<<"$input")
pct=$(jq -r '.context_window.used_percentage // 0' <<<"$input" | cut -d. -f1)

branch=""
[[ -n "$dir" ]] && branch=$(git -C "$dir" --no-optional-locks branch --show-current 2>/dev/null)
[[ -n "$branch" ]] && branch=" ⎇ $branch"

color() { if (( $1 >= 75 )); then printf '\033[31m'; elif (( $1 >= 50 )); then printf '\033[33m'; else printf '\033[32m'; fi; }

bar=""; for ((i=0;i<pct/10;i++)); do bar+="█"; done; for ((i=pct/10;i<10;i++)); do bar+="░"; done
out=$(printf '[%s]%s | %s%s %d%%\033[0m ctx' "$model" "$branch" "$(color "$pct")" "$bar" "$pct")

# Subscription quotas (absent on API-key billing). Reset shown as HH:MM for
# the 5h window and weekday+HH:MM (locale abbreviation) for the 7d window.
fmt_reset() { date -d "@$1" +"$2" 2>/dev/null || date -r "$1" +"$2"; }
quota() {  # quota <label> <json-path> <date-format>
  local used reset
  used=$(jq -r ".rate_limits.$2.used_percentage // empty" <<<"$input" | cut -d. -f1)
  [[ -n "$used" ]] || return 0
  reset=$(jq -r ".rate_limits.$2.resets_at // empty" <<<"$input")
  [[ -n "$reset" ]] && reset=" ↻$(fmt_reset "$reset" "$3")"
  out+=$(printf ' | [%s] %s%d%%\033[0m%s' "$1" "$(color "$used")" "$used" "$reset")
}
quota 5h five_hour '%H:%M'
quota 7d seven_day '%a%H:%M'

printf '%b\n' "$out"
