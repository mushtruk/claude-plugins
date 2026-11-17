# claude-plugins

Claude Code plugins.

```
/plugin marketplace add mushtruk/claude-plugins
/plugin install handoff@mushtruk
/plugin install driftguard@mushtruk
```

Then restart Claude Code. A private repo clones over your GitHub SSH key (it must work without a passphrase prompt) or a stored HTTPS credential. Updates are off by default: `/plugin` → Marketplaces → mushtruk → Enable auto-update, or `/plugin marketplace update mushtruk`.

## handoff

Long sessions lose track once the context fills up and gets compacted. This plugin makes Claude write a handoff file first, then reload it after a compaction.

| Piece | What it does |
|---|---|
| `/handoff:handoff [focus]` | Writes `<repo>/.claude/handoffs/<YYYY-MM-DD-HHMM>-<slug>.md` (state, next step, open questions, a kickoff paragraph) and ends the turn in the same chat, leaving running agents running and starting no new work; compaction re-injects it. Only when you ask for a new chat does it copy the file to the clipboard (`pbcopy` on macOS, `wl-copy` on Wayland; elsewhere it prints the text instead) for you to paste. |
| Stop hook | When the main thread ends a turn at 100k+ tokens, it blocks once and asks Claude to write or refresh the handoff file, listing running subagents and background work as in flight with their output files (the message names the agents the transcript shows as launched with no result yet), then to keep working in the same chat: no stopping, no waiting, no "open a new chat". The message ends with the handoff recorded for this session, or says none is recorded and that only a Write-tool file is re-injected. A compaction or `/clear` re-arms it. |
| PostToolUse hook | When a subagent passes 120k tokens, it tells it once to finish its step and hand off. When the main thread writes or edits a file directly in `.claude/handoffs/` (not `agent-*`), it records that file as this session's handoff. |
| SessionStart, SessionEnd hooks | After a compaction, it injects the handoff this session recorded, if it was written in the last 3 hours; after a `/clear`, the one the cleared session recorded (SessionEnd notes which session `/clear` ended in that directory). It never falls back to another file, so chats sharing a directory never load each other's handoff. After a compaction it follows with the agents still running from before it (up to 20). On every session start it also reaps orphaned background processes (below). |
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

## driftguard

Sessions working in parallel worktrees drift apart: two branches rework the same files, or the default branch moves under one of them, and nobody notices until a large merge conflict. This plugin tells each session, early, which files its branch shares with origin's default branch and with sibling branches, while the overlap is still small.

| Piece | What it does |
|---|---|
| SessionStart hook | Reports every standing overlap: upstream commits on origin's default branch that change files this branch also changes (with a merge or rebase command), and each sibling branch, local or pushed, that changes the same files. Every new, resumed, cleared or compacted context hears the standing overlap once, since the earlier notice is gone from its context. |
| UserPromptSubmit hook | Checks the worktree at most every 30 minutes and speaks only when there is news: a new upstream commit or a sibling touching one more shared file. Otherwise it stays silent. |

The hooks run `driftguard`, a small Go binary (source in `plugins/driftguard/src/driftguard`, builds for macOS and Linux on arm64 and amd64 in `bin/`). It fetches origin's default branch at most every 30 minutes per repo, and falls back to local refs, with a note that they may be stale, when the fetch fails. It never fails a hook: on any error it exits 0 silently and logs why.

**Scope.** driftguard reads only git metadata about other branches: their names, pushed state and changed file names. It only ever advises actions inside the session's own branch: merge or rebase, adapt the branch, or contact the other branch's session.

**Coverage.** The hook sees only the worktree of the session's `cwd`. A session on the default branch gets no overlap report. Subagents fire neither hook; they learn through the main session's briefs. Without `refs/remotes/origin/HEAD` driftguard stays silent; `git remote set-head origin --auto` fixes it.

### Settings

| Variable | Default | |
|---|---|---|
| `DRIFTGUARD_STATE_DIR` | `~/.claude/state/driftguard` | Per-worktree fingerprints of what each session was told, per-repo fetch times, and `log.jsonl`, one line per hook run (capped at 1 MiB plus one rotated file) |

### Rebuild

```sh
cd plugins/driftguard/src/driftguard && go test ./...
for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags='-s -w -buildid=' -o ../../bin/driftguard-${t%/*}-${t#*/} .
done
```
