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
	var (
		rep   report
		files paths
		quiet = "quiet"
	)
	if branch == def {
		// The default branch overlaps nothing of its own; its uncommitted
		// files still hear the board.
		quiet = "default-branch"
		files, err = r.uncommittedFiles(ctx)
	} else {
		rep, files, err = w.overlap(ctx, r, branch, def, now)
	}
	if err != nil {
		return "", "", err
	}
	e, err := w.boardEntries(r)
	if err != nil {
		return "", "", err
	}
	live, err := r.liveClaims(ctx, def, e)
	if err != nil {
		return "", "", err
	}
	rep.claims = claimsOver(live, branch, files)
	recent := e.recentAnnouncements(now)
	for _, a := range recent {
		if a.concerns(branch, files) {
			rep.announcements = append(rep.announcements, a)
		}
	}
	prev := st.Fingerprint
	if ev == sessionStart {
		// A new or compacted context has lost every earlier notice.
		prev = fingerprint{}
	} else {
		rep.announcements = st.unseen(rep.announcements)
	}
	text, shown := render(branch, def, prev.news(rep))
	st.Fingerprint = fingerprintOf(rep)
	st.see(shown, recent)
	if err := w.states.save(r.root, st); err != nil {
		return "", "", err
	}
	if text == "" {
		return "", quiet, nil
	}
	return text, "spoke", nil
}

// boardEntries reads the repo's board, logging the lines it skipped.
func (w watcher) boardEntries(r repo) (entries, error) {
	e, skipped, err := r.board().read()
	if err != nil {
		return entries{}, err
	}
	if skipped > 0 {
		w.log.Warn("board lines skipped", "root", r.root, "count", skipped)
	}
	return e, nil
}

// overlap fetches the default branch when the repo's fetch window allows and
// reports what this branch shares with it and with sibling branches, and
// the files the branch changes.
func (w watcher) overlap(ctx context.Context, r repo, branch, def string, now time.Time) (report, paths, error) {
	var rep report
	if now.Sub(w.states.lastFetch(r.commonDir)) >= window {
		if err := w.states.saveFetch(r.commonDir, now); err != nil {
			return report{}, nil, err
		}
		if err := r.git.fetch(ctx, r.root, def); err != nil {
			rep.stale = true
			w.log.Warn("fetch failed", "root", r.root, "error", err.Error())
		}
	}
	uncommitted, err := r.uncommittedFiles(ctx)
	if err != nil {
		return report{}, nil, err
	}
	files, err := r.branchFiles(ctx, def, uncommitted)
	if err != nil {
		return report{}, nil, err
	}
	commits, err := r.upstream(ctx, def)
	if err != nil {
		return report{}, nil, err
	}
	pushed, err := r.pushed(ctx)
	if err != nil {
		return report{}, nil, err
	}
	own := branchWork{branch: branch, uncommitted: uncommitted}
	sibs, skipped, err := r.siblings(ctx, def, own, pushed)
	if err != nil {
		return report{}, nil, err
	}
	for b, err := range skipped {
		w.log.Warn("sibling skipped", "root", r.root, "branch", b, "error", err.Error())
	}
	rep.upstream = upstreamOverlap(commits, files)
	rep.siblings = sibs
	rep.pushed = pushed[branch]
	return rep, files, nil
}
