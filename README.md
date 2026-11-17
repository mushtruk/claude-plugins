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
| `/handoff:handoff [focus]` | Writes `<repo>/.claude/handoffs/<YYYY-MM-DD-HHMM>-<slug>.md` (state, next step, open questions, a kickoff paragraph) and ends the turn in the same chat, leaving running agents running and starting no new work; compaction re-injects it. Only when you ask for a new chat does it copy the file to the clipboard (`pbcopy` on macOS, `wl-copy` on Wayland; elsewhere it prints the text instead) for you to paste. |
| Stop hook | When the main thread ends a turn at 100k+ tokens, it blocks once and asks Claude to write or refresh the handoff file, listing running subagents and background work as in flight with their output files (the message names the agents the transcript shows as launched with no result yet), then to keep working in the same chat: no stopping, no waiting, no "open a new chat". The message ends with the handoff recorded for this session, or says none is recorded and that only a Write-tool file is re-injected. A compaction or `/clear` re-arms it. |
| PreToolUse hook | Refuses a subagent's `Read` or `Bash` read that would return more than 8 KB, and tells it how many lines it may read instead (below). The main thread is never limited. |
| PostToolUse hook | When a subagent passes 120k tokens, it tells it once to finish its step and hand off. When the main thread writes or edits a file directly in `.claude/handoffs/` (not `agent-*`), it records that file as this session's handoff. |
| SessionStart, SessionEnd hooks | After a compaction, it injects the handoff this session recorded, if it was written in the last 3 hours; after a `/clear`, the one the cleared session recorded (SessionEnd notes which session `/clear` ended in that directory). It never falls back to another file, so chats sharing a directory never load each other's handoff. After a compaction it follows with the agents still running from before it (up to 20). On every session start it also reaps orphaned background processes (below). |
| `handoff:lean-implementer`, `handoff:lean-reviewer` | Lean subagents for plan-driven work. They know the "CONTEXT LIMIT" message: the implementer writes `.claude/handoffs/agent-<task>-<n>.md` and returns `Status: HANDOFF <path>`; the reviewer returns `Status: PARTIAL`. Dispatch a continuation with the handoff file's text pasted into its prompt, not the path, so the new agent opens nothing to start. |

The hooks run `ctxguard`, a small Go binary (source in `plugins/handoff/src/ctxguard`, builds for macOS and Linux on arm64 and amd64 in `bin/`). It never fails a hook: on any error it exits 0 silently.

**Read cap.** Everything a subagent reads is paid again on each of its later turns, so a read over `CTXGUARD_AGENT_READ_CAP` bytes (default 8192) is denied before it runs, with a message to find the place with `grep -n` and read the line count that fits, or, when one line alone is over the cap, to take part of it with `cut -c`. It applies to every subagent, not only the lean ones: any subagent's oversized read costs the same. The size is an estimate: a `Read` counts the requested lines (from `offset`, `limit` or 2000 lines) plus a line-number prefix each. In `Bash` it recognises only `cat F...`, `nl F`, `head`/`tail` with `-n N` or `-N`, and `sed -n 'A,Bp' F` (or `'Ap'`), joined by `;` or `&&` and optionally after `cd DIR`, summing them. Anything else is allowed unmeasured: pipes, redirects, globs, variables, other quoting or flags (`cat -n`, `tail -n +N`), and every other command. Images, PDFs, notebooks, `Read` with `pages`, binary files (a NUL in the first 8 KB) and anything but a regular file are not counted, and any error allows the call. `CTXGUARD_AGENT_READ_CAP=0` turns the cap off.

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
| `CTXGUARD_AGENT_READ_CAP` | `8192` | Bytes a subagent read may return before it is refused; `0` turns the cap off |
| `CTXGUARD_STATE_DIR` | `~/.claude/state/ctxguard` | One-shot markers, so each session or agent is told once, and each session's handoff record (pruned with reaping, after the idle window) |
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
