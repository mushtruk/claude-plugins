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
| `/handoff:handoff [focus]` | Writes `<repo>/.claude/handoffs/<YYYY-MM-DD-HHMM>-<slug>.md` (state, next step, open questions, a kickoff paragraph) and keeps working in the same chat; compaction re-injects it. Only when you ask for a new chat does it copy the file to the clipboard (`pbcopy` on macOS, `wl-copy` on Wayland; elsewhere it prints the text instead) for you to paste. |
| Stop hook | When the main thread ends a turn at 100k+ tokens, it blocks once and asks Claude to write or refresh the handoff file, listing running subagents and background work as in flight with their output files (the message names the agents the transcript shows as launched with no result yet), then to keep working in the same chat: no stopping, no waiting, no "open a new chat". A compaction or `/clear` re-arms it. |
| PostToolUse hook | When a subagent passes 120k tokens, it tells it once to finish its step and hand off. |
| SessionStart hook | After a compaction or a `/clear`, it injects the newest handoff from the last 3 hours (agent-* files skipped) into the new context; after a compaction it follows with the agents still running from before it (up to 20). On every session start it also reaps orphaned background processes (below). |
| `handoff:lean-implementer`, `handoff:lean-reviewer` | Lean subagents for plan-driven work. They know the "CONTEXT LIMIT" message: the implementer writes `.claude/handoffs/agent-<task>-<n>.md` and returns `Status: HANDOFF <path>`; the reviewer returns `Status: PARTIAL`. |

The hooks run `ctxguard`, a small Go binary (source in `plugins/handoff/src/ctxguard`, builds for macOS and Linux on arm64 and amd64 in `bin/`). It never fails a hook: on any error it exits 0 silently.

**Reaping.** Background processes started from a session's temp dir (`/tmp/claude-<uid>/<project>/<session>/`) outlive the session when their shell exits. On each session start, ctxguard sends SIGTERM to such a process only when every check proves it abandoned: your uid, reparented to init, not Claude itself, not the current session, older than the idle window, and its session transcript untouched for that long. Anything unproven is skipped. `ctxguard reap --dry-run` lists every candidate with its verdict.

### Setup

**Keep handoffs out of git.** Handoffs land inside your repos. Add them to your global gitignore:

```sh
git config --global core.excludesFile   # prints your file if you already have one; use it below
git config --global core.excludesFile ~/.config/git/ignore
mkdir -p ~/.config/git && echo '.claude/handoffs/' >> ~/.config/git/ignore
```

**Status line (optional).** Plugins can't set it, so add this to `~/.claude/settings.json`. It shows `ctx 87k` in green, red with "handoff" from 100k (the main limit; the yellow band from 100k is hidden while the limit is 100k):

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
| `CTXGUARD_MAIN_LIMIT` | `100000` | Main-thread tokens at which the Stop hook asks for a handoff. Keep it at least 15k below where compaction really fires: with `autoCompactWindow: 170000` that was measured at about 129-137k, well under the window itself. |
| `CTXGUARD_AGENT_LIMIT` | `120000` | Subagent tokens at which it is told to hand off |
| `CTXGUARD_STATE_DIR` | `~/.claude/state/ctxguard` | One-shot markers, so each session or agent is told once |
| `CTXGUARD_LOG` | unset | A file to log every hook decision and reap verdict to, for debugging |
| `CTXGUARD_REAP` | unset | `0` turns reaping off |
| `CTXGUARD_REAP_IDLE` | `72h` | How long a process and its session's transcript must be idle before it is reaped (a Go duration) |

### Rebuild

```sh
cd plugins/handoff/src/ctxguard && go test ./...
for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags='-s -w -buildid=' -o ../../bin/ctxguard-${t%/*}-${t#*/} .
done
```
