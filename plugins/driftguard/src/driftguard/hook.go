package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const (
	// window is the least time between two checks of a worktree on prompts,
	// and between two fetches of a repo.
	window = 30 * time.Minute
	// runTimeout bounds a whole run; the hook's own timeout leaves room past
	// it to stop git and exit.
	runTimeout = 6 * time.Second
	// maxInput bounds the hook input, which carries the user's prompt.
	maxInput = 16 << 20
)

type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	Cwd           string `json:"cwd"`
}

// event is a hook event driftguard answers.
type event int

const (
	sessionStart event = iota + 1
	userPromptSubmit
)

func parseEvent(name string) (event, bool) {
	switch name {
	case "SessionStart":
		return sessionStart, true
	case "UserPromptSubmit":
		return userPromptSubmit, true
	}
	return 0, false
}

func (e event) String() string {
	if e == sessionStart {
		return "SessionStart"
	}
	return "UserPromptSubmit"
}

// watcher answers SessionStart and UserPromptSubmit hooks with what is new in
// the overlap between the session's branch and other work.
type watcher struct {
	git    gitRunner
	now    func() time.Time
	states states
	log    *slog.Logger
}

// hook never fails loudly: whatever goes wrong, it prints nothing and logs
// one line.
func (w watcher) hook(in io.Reader, out io.Writer) {
	start := w.now()
	var (
		h        hookInput
		decision string
		err      error
	)
	defer func() {
		if p := recover(); p != nil {
			decision, err = "panic", fmt.Errorf("panic: %v", p)
		}
		attrs := []any{"event", h.HookEventName, "cwd", h.Cwd, "decision", decision, "duration_ms", w.now().Sub(start).Milliseconds()}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		w.log.Info("hook", attrs...)
	}()

	if err = json.NewDecoder(io.LimitReader(in, maxInput)).Decode(&h); err != nil {
		decision = "bad-input"
		return
	}
	ev, ok := parseEvent(h.HookEventName)
	if !ok {
		decision = "ignored-event"
		return
	}
	if h.Cwd == "" {
		decision = "no-cwd"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	var text string
	text, decision, err = w.check(ctx, ev, h.Cwd)
	if err != nil {
		decision = "error"
		return
	}
	if text == "" {
		return
	}
	err = writeJSON(out, map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     ev.String(),
			"additionalContext": text,
		},
	})
}

// check returns the notice for the worktree at cwd, if any, and the decision
// to log.
func (w watcher) check(ctx context.Context, ev event, cwd string) (text, decision string, err error) {
	r, err := openRepo(ctx, w.git, cwd)
	if err != nil {
		return "", "", err
	}
	st, err := w.states.load(r.root)
	if err != nil {
		w.log.Warn("state unreadable, starting over", "root", r.root, "error", err.Error())
	}
	now := w.now().UTC()
	if ev == userPromptSubmit && now.Sub(st.LastCheck) < window {
		return "", "throttled", nil
	}
	branch, err := r.branch(ctx)
	if errors.Is(err, errDetached) {
		return "", "detached", nil
	}
	if err != nil {
		return "", "", err
	}
	def, err := r.defaultBranch(ctx)
	if errors.Is(err, errNoOriginHead) {
		return "", "no-origin-head", nil
	}
	if err != nil {
		return "", "", err
	}
	// Saved first, so a run that fails later still waits a full window.
	st.LastCheck = now
	if err := w.states.save(r.root, st); err != nil {
		return "", "", err
	}
	if branch == def {
		return "", "default-branch", nil
	}

	rep, err := w.overlap(ctx, r, branch, def, now)
	if err != nil {
		return "", "", err
	}
	prev := st.Fingerprint
	if ev == sessionStart {
		// A new or compacted context has lost every earlier notice.
		prev = fingerprint{}
	}
	text = render(branch, def, prev.news(rep))
	st.Fingerprint = fingerprintOf(rep)
	if err := w.states.save(r.root, st); err != nil {
		return "", "", err
	}
	if text == "" {
		return "", "quiet", nil
	}
	return text, "spoke", nil
}

// overlap fetches the default branch when the repo's fetch window allows and
// reports what this branch shares with it and with sibling branches.
func (w watcher) overlap(ctx context.Context, r repo, branch, def string, now time.Time) (report, error) {
	var rep report
	if now.Sub(w.states.lastFetch(r.commonDir)) >= window {
		if err := w.states.saveFetch(r.commonDir, now); err != nil {
			return report{}, err
		}
		if err := r.git.fetch(ctx, r.root, def); err != nil {
			rep.stale = true
			w.log.Warn("fetch failed", "root", r.root, "error", err.Error())
		}
	}
	uncommitted, err := r.uncommittedFiles(ctx)
	if err != nil {
		return report{}, err
	}
	files, err := r.branchFiles(ctx, def, uncommitted)
	if err != nil {
		return report{}, err
	}
	commits, err := r.upstream(ctx, def)
	if err != nil {
		return report{}, err
	}
	pushed, err := r.pushed(ctx)
	if err != nil {
		return report{}, err
	}
	own := branchWork{branch: branch, files: files, uncommitted: uncommitted}
	sibs, skipped, err := r.siblings(ctx, def, own, pushed)
	if err != nil {
		return report{}, err
	}
	for b, err := range skipped {
		w.log.Warn("sibling skipped", "root", r.root, "branch", b, "error", err.Error())
	}
	rep.upstream = upstreamOverlap(commits, files)
	rep.siblings = sibs
	rep.pushed = pushed[branch]
	return rep, nil
}
