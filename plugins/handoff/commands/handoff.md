---
description: Use when the context is getting large (status line yellow or red, around 115k or more), at a clean stopping point, or before moving work to a fresh chat
argument-hint: "[optional focus, e.g. 'the telemetry bug']"
allowed-tools: Bash(git branch:*), Bash(git status:*), Bash(git log:*), Bash(git diff:*), Bash(git -C:*), Bash(git rev-list:*), Bash(git stash list:*), Bash(pbcopy:*), Bash(date:*), Read, Grep, Glob, Write(./.claude/handoffs/**)
---

# Handoff

**The reader is you, cold:** a fresh chat that has your memory notes and the repos but none of this conversation, and whose first message is this handoff. Give it what it needs to take the next correct action without re-deriving anything or re-asking the user. Leave out anything git, the code, the plan or memory already tells it.

$ARGUMENTS: if given, weight the handoff toward this.

## 1. Settle, before writing
- **Running work** (agents, background shells, watchers) dies with this chat. If it finishes within minutes, wait for it. Otherwise stop it and record where it stood: the last completed step and the files it touched.
- **Durable facts** (user decisions and preferences, rules, findings that outlive the next piece): write them to memory or the plan doc now, following your memory rules. The handoff points to them; it is not storage.

## 2. Gather, in every repo this session touched
That's the primary directory plus any additional directory where you edited, committed or ran tests.
```
git -C <repo> branch --show-current; git -C <repo> status --short; git -C <repo> log --oneline -8
git -C <repo> diff --stat; git -C <repo> stash list
git -C <repo> rev-list --left-right --count @{u}...HEAD    # "behind ahead"; if it errors: no upstream
```
Git beats memory: if they disagree, say so. Sort dirty and untracked files into *expected* (never committed) and *real work*.

## 3. Verify
Check every file:line, symbol, hash, path, command and number before it goes in, using Grep, Read or git. If you can't verify something, mark it `(unverified)` or drop it.
- Line numbers only when checked in this step. Otherwise cite the symbol (`deployModels` in module_migration.go). Never "about L…": a near-miss anchor misleads more than none.
- Label each commit by what `git log` says it is, not by the piece you were working on.
- Absolute dates only.

## 4. Write it to a file
Save to `<primary repo>/.claude/handoffs/<YYYY-MM-DD-HHMM>-<slug>.md`, where the primary repo is the chat's working directory and the slug is 2–4 words of the title. Keep `.claude/handoffs/` out of git (a global gitignore entry; see the plugin README). Write plain text in this shape: no outer fence, inner code fences are fine. A long session needs about 600–900 words, a short one a few lines. Omit empty sections.

```text
Handoff: <title> · <YYYY-MM-DD>
<repo>: <branch> @ <hash> · <clean | dirty: files, expected or real work> · <in sync | N unpushed | no upstream>

Goal: <what, and why, the intent the code doesn't show>
Done: <item, its commit or how it was verified>
In progress: <file, step, last command and its result> | nothing
Next: <the single next action>
Left: <what remains; what's blocked and on what>

Waiting on the user:
- <question, verbatim> Options: <a / b>. Recommend: <x>, because <why>. <Gates Next | Doesn't gate Next>.

Not in the code:
- <decision, why (memory: note-name)>
- <root cause, evidence>
- <dead end, why not to retry>

Rules in force (only those that bind the next actions; at most ~6 lines):
- <rule (memory: note-name)>

Kickoff:
<one paragraph: repo and branch; the first concrete action, putting work that doesn't need the user's answer ahead of questions that don't gate it; files to read first (path:line); constraints; what to report back>
```

Before saving, check:
- Could a cold reader do **Next** with only this text and the repos? Remove "as discussed", any id from a store it can't see (decision or question numbers, run ids, task ids), and any tool or command it may not have.
- Nothing restates what memory, the plan or git already holds; use a pointer instead.
- No secrets, no file dumps, no relative dates.

## 5. Deliver
Run `pbcopy < <file>`, so the full text is on the clipboard and the new chat gets it from one paste with no file permission prompt. Then reply with only these lines:

```text
Handoff saved: [<file name>](<path>) · copied to clipboard. Open a new chat and paste (⌘V).
Next: <the single next action> · Waiting on you: <questions, each marked gates or doesn't gate Next> | nothing
Persisted: <what step 1 wrote, and where> | nothing
```
If `pbcopy` fails, say so and print the file's contents in a block fenced with four backticks (````text) instead.
