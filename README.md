# claude-plugins

Claude Code plugins.

```
/plugin marketplace add mushtruk/claude-plugins
/plugin install handoff@mushtruk
```

Then restart Claude Code. A private repo clones over your GitHub SSH key (it must work without a passphrase prompt) or a stored HTTPS credential. Updates are off by default: `/plugin` → Marketplaces → mushtruk → Enable auto-update, or `/plugin marketplace update mushtruk`.

## handoff

Long sessions lose track once the context fills up and gets compacted. This plugin makes Claude write a handoff file first, then reload it after a compaction.

| Piece | What it does |
|---|---|
| `/handoff:handoff [focus]` | Writes `<repo>/.claude/handoffs/<YYYY-MM-DD-HHMM>-<slug>.md` (state, next step, open questions, a kickoff paragraph) and copies it to the clipboard (`pbcopy`; on Linux it prints the text instead). Paste it into a fresh chat. |
| Stop hook | When the main thread ends a turn at 150k+ tokens, it blocks once and asks Claude to run the handoff. |
| PostToolUse hook | When a subagent passes 120k tokens, it tells it once to finish its step and hand off. |
| SessionStart hook | After a compaction, it injects the newest handoff from the last 3 hours (agent-* files skipped) into the new context. |
| `handoff:lean-implementer`, `handoff:lean-reviewer` | Lean subagents for plan-driven work. They know the "CONTEXT LIMIT" message: the implementer writes `.claude/handoffs/agent-<task>-<n>.md` and returns `Status: HANDOFF <path>`; the reviewer returns `Status: PARTIAL`. |

The hooks run `ctxguard`, a small Go binary (source in `plugins/handoff/src/ctxguard`, builds for macOS and Linux on arm64 and amd64 in `bin/`). It reads only the session transcript and never fails a hook: on any error it exits 0 silently.

### Setup

**Keep handoffs out of git.** Handoffs land inside your repos. Add them to your global gitignore:

```sh
git config --global core.excludesFile   # prints your file if you already have one; use it below
git config --global core.excludesFile ~/.config/git/ignore
mkdir -p ~/.config/git && echo '.claude/handoffs/' >> ~/.config/git/ignore
```

**Status line (optional).** Plugins can't set it, so add this to `~/.claude/settings.json`. It shows `ctx 87k` in green, yellow from 100k, red with "handoff" from 150k:

```json
"statusLine": {
  "type": "command",
  "command": "sh -c 'd=$(ls -td ~/.claude/plugins/cache/mushtruk/handoff/*/ 2>/dev/null | head -1); [ -n \"$d\" ] && exec sh \"${d}bin/ctxguard\" statusline'"
}
```

It picks the newest installed version, so it survives plugin updates.

### Settings

Set these in the `env` block of `~/.claude/settings.json`:

| Variable | Default | |
|---|---|---|
| `CTXGUARD_MAIN_LIMIT` | `150000` | Main-thread tokens at which the Stop hook asks for a handoff |
| `CTXGUARD_AGENT_LIMIT` | `120000` | Subagent tokens at which it is told to hand off |
| `CTXGUARD_STATE_DIR` | `~/.claude/state/ctxguard` | One-shot markers, so each session or agent is told once |
| `CTXGUARD_LOG` | unset | A file to log every hook decision to, for debugging |

### Rebuild

```sh
cd plugins/handoff/src/ctxguard && go test ./...
for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags='-s -w -buildid=' -o ../../bin/ctxguard-${t%/*}-${t#*/} .
done
```
