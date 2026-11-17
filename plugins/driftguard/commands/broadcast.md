---
description: Use when your branch changes or is about to rework a contract other branches build on, when it lands, or when driftguard warns about another branch's work
argument-hint: "[what changed or what you will own]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/bin/driftguard post:*), Bash(${CLAUDE_PLUGIN_ROOT}/bin/driftguard claim:*), Bash(${CLAUDE_PLUGIN_ROOT}/bin/driftguard release:*), Bash(git diff:*), Bash(git log:*), Bash(git status:*), Bash(git branch:*)
---

# Broadcast

**The readers are the sessions on sibling branches:** they see only your announcement or claim, never this conversation. Tell them what they must know to keep building without drifting from your branch.

$ARGUMENTS: if given, it is what changed or what you will own.

## 1. Decide
- **Claim** at plan time, before building, when the work will rework a shared surface: an exported Go API, a proto, a schema or migration, a config key, an event or message shape, an error kind, a shared helper.
- **Announce** when a change alters something another branch calls or reads: once per contract change, as soon as the new shape is settled.
- **Release** when the branch lands or drops the surface. Landing also ends a claim on its own.
- Don't post internal refactors, or changes that git overlap already shows to the only affected branch.

## 2. Write
- **Summary**: the contract or the owned surface, in one or two sentences. Not the history.
- **Action** (announcements only): what a consumer does to adapt.
- **Paths**: the repo-relative prefixes or globs a consumer's files would touch: the changed package directory, not your whole branch. `*` does not cross `/`. A claim always has paths; an announcement omits them only for a repo-wide change.
- Bounds: summary and action at most 300 characters each; at most 10 paths of at most 200 bytes each.

## 3. Post
Run from this worktree, with the full launcher path:
```
${CLAUDE_PLUGIN_ROOT}/bin/driftguard post --summary "..." --action "..." --paths gopkg/mailer,proto/fga/mail
${CLAUDE_PLUGIN_ROOT}/bin/driftguard claim --summary "..." --paths gopkg/mailer
${CLAUDE_PLUGIN_ROOT}/bin/driftguard release
```
- If it prints `entry too long`, shorten as it says and run it again.
- If `claim` prints that another branch already claims an overlapping surface, settle ownership with that session before building.

## 4. React to a warning
- Build on the owner's announced or claimed shape; don't add a parallel one.
- To settle ownership or timing, find the owning branch's session with ListAgents and ask it with SendMessage.
- Never edit, merge, rebase, push or check out another branch, and never enter another worktree.

## 5. Deliver
Reply with only the line that fits, then end the turn:
```text
Posted: <id> · <summary>
Claimed: <id> · <summary>
Released: <N> claims
```
After `Claimed:`, add any overlap line `claim` printed. If the command printed no `posted`, `claimed` or `released` line, reply only `driftguard has no build for this platform; nothing was posted.`
