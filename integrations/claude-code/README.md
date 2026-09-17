# Claude Code Integration

The Agency was built for Claude Code. No conversion needed — agents work
natively with the existing `.md` + YAML frontmatter format.

## Install

```bash
# Copy all agents to your Claude Code agents directory
./scripts/install.sh --tool claude-code

# Or manually copy a category
cp engineering/*.md ~/.claude/agents/
```

## Status Line

`install.sh --tool claude-code` also drops `statusline.sh` into `~/.claude/` and
wires it into `settings.json` (only if no `statusLine` is set yet). It shows:

```
[Opus 5] ⎇ main | ████░░░░░░ 42% ctx | [5h] 34% ↻14:30 | [7d] 12% ↻mon13:00
```

Context bar goes yellow at 50% and red at 75% — the cue to open a fresh session
and run `/maestro`; the pipeline ledger brings the Maestro back where it stopped.
`[5h]` / `[7d]` are the subscription rate limits with their reset time (hidden on API-key billing).
Requires `jq`.

## Activate an Agent

In any Claude Code session, reference an agent by name:

```
Activate Frontend Developer and help me build a React component.
```

```
Use the Reality Checker agent to verify this feature is production-ready.
```

## Agent Directory

Agents are organized into divisions. See the [main README](../../README.md) for
the full Agency roster.
