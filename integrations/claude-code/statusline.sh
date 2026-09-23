#!/usr/bin/env bash
# Agency status line for Claude Code.
#   [Opus 5] ████░░░░░░ 42% ctx | [5h] ███░░░░░░░ 34% ↻14:30 | [7d] █░░░░░░░░░ 12% ↻mon13:00
#   /srv/app <main>
# Context bar turns yellow at 50% and red at 75% — the cue to start a fresh
# session and run /maestro (the ledger brings the pipeline back). The 5h / 7d
# figures are the subscription rate limits; absent on API-key billing.
#
# Path and branch get their own row: an absolute path is as long as it is, and
# crowding it onto the meters line pushes them out of view on a narrow terminal.
# They answer "where am I", not "how much is left".
#
# WHY THE TIMER, AND WHY THE mtime GATE
# Claude Code re-runs a status line when a new ASSISTANT MESSAGE arrives. A
# coordinator waiting on background subagents produces none — and those subagents
# spend the SAME 5h/7d quota, so both bars sat still exactly when they were moving
# fastest. No hook can force a refresh, so the installer sets a short
# `refreshInterval`; each tick is what brings fresh `rate_limits` in.
#
# A short tick only stays cheap if an idle tick is cheap. `git branch` is the
# expensive part, so it runs only when something actually happened: the newest
# mtime across THIS session's transcript and its subagents'. Idle ticks reuse the
# cached branch. The mtime cannot be the trigger — nothing is running between
# ticks to look at it — but it decides whether a tick does any work.
# Installed by scripts/install.sh --tool claude-code; fields documented at
# https://code.claude.com/docs/en/statusline
input=$(cat)

# One jq for every field.
IFS=$'\t' read -r model dir pct sid transcript five_u five_r seven_u seven_r <<<"$(
  jq -r '[ .model.display_name // "?",
           .workspace.current_dir // "",
           (.context_window.used_percentage // 0 | floor),
           .session_id // "",
           .transcript_path // "",
           (.rate_limits.five_hour.used_percentage // "" | if . == "" then "" else floor end),
           .rate_limits.five_hour.resets_at // "",
           (.rate_limits.seven_day.used_percentage // "" | if . == "" then "" else floor end),
           .rate_limits.seven_day.resets_at // ""
         ] | @tsv' <<<"$input"
)"

color() { if (( $1 >= 75 )); then printf '\033[31m'; elif (( $1 >= 50 )); then printf '\033[33m'; else printf '\033[32m'; fi; }

bar() { local b="" i; for ((i=0;i<$1/10;i++)); do b+="█"; done; for ((i=$1/10;i<10;i++)); do b+="░"; done; printf '%s' "$b"; }
out=$(printf '[%s] %s%s %d%%\033[0m ctx' "$model" "$(color "$pct")" "$(bar "$pct")" "$pct")

# Subscription quotas (absent on API-key billing). Reset shown as HH:MM for
# the 5h window and weekday+HH:MM (locale abbreviation) for the 7d window.
fmt_reset() { date -d "@$1" +"$2" 2>/dev/null || date -r "$1" +"$2"; }
quota() {  # quota <label> <used> <resets_at> <date-format>
  local reset=""
  [[ -n "$2" ]] || return 0
  [[ -n "$3" ]] && reset=" ↻$(fmt_reset "$3" "$4")"
  out+=$(printf ' | [%s] %s%s %d%%\033[0m%s' "$1" "$(color "$2")" "$(bar "$2")" "$2" "$reset")
}
quota 5h "$five_u" "$five_r" '%H:%M'
quota 7d "$seven_u" "$seven_r" '%a%H:%M'

# The activity signal: newest write by me or by any of my subagents. The subagent
# transcripts live under the scratchpad for this session, whose slug is the
# transcript's own directory name.
atividade() {
  local slug tasks
  [[ -n "$transcript" ]] || { printf '0'; return; }
  slug=$(basename "$(dirname "$transcript")")
  tasks="${TMPDIR:-/tmp}/claude-$(id -u)/$slug/$sid/tasks"
  find "$transcript" "$tasks" -type f -printf '%T@\n' 2>/dev/null |
    awk 'BEGIN{m=0} $1>m{m=$1} END{printf "%d", m}'
}

# Where I am: the absolute path, and the branch OF THAT PATH. The branch is read
# only when the activity signal moved; otherwise the cache answers. Keyed by
# session_id — stable for the session, unique across them. `$$` would change on
# every invocation and defeat the cache.
linha2=""
if [[ -n "$dir" ]]; then
  linha2="$dir"
  cache="${TMPDIR:-/tmp}/agency-statusline-$sid"
  agora=$(atividade)
  # The newline matters: `read` returns non-zero on EOF without a delimiter, and
  # a false read would send every tick down the git path — a cache that looks
  # present and never hits.
  visto=""; cached=""
  [[ -r "$cache" ]] && IFS='|' read -r visto cached < "$cache"
  if [[ -n "$visto" && "$visto" == "$agora" ]]; then
    branch="$cached"
  else
    branch=$(git -C "$dir" --no-optional-locks branch --show-current 2>/dev/null)
    printf '%s|%s\n' "$agora" "$branch" > "$cache" 2>/dev/null || true
  fi
  [[ -n "$branch" ]] && linha2+=" <$branch>"
fi

printf '%b\n' "$out"
[[ -n "$linha2" ]] && printf '%s\n' "$linha2"
