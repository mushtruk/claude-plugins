package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// rig is a watcher over a fixture, with a fake clock and an in-memory log.
type rig struct {
	f     *fixture
	clock *fakeClock
	log   *bytes.Buffer
	w     watcher
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := newFixture(t)
	r := &rig{
		f:     f,
		clock: &fakeClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)},
		log:   &bytes.Buffer{},
	}
	r.w = watcher{
		git:    f.runner(),
		now:    r.clock.now,
		states: states{dir: t.TempDir()},
		log:    slog.New(slog.NewJSONHandler(r.log, nil)),
	}
	return r
}

func startEvent(cwd string) map[string]any {
	return map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s1", "cwd": cwd}
}

func promptEvent(cwd string) map[string]any {
	return map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "prompt": "secret prompt text", "cwd": cwd}
}

// runHook feeds ev to the hook and returns the context it adds, "" when it
// says nothing.
func runHook(t *testing.T, w watcher, ev map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	out := hookRaw(w, string(raw))
	if out == "" {
		return ""
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	hso, _ := resp["hookSpecificOutput"].(map[string]any)
	if got := hso["hookEventName"]; got != ev["hook_event_name"] {
		t.Fatalf("hookEventName = %v, want %v", got, ev["hook_event_name"])
	}
	return additional(resp)
}

func additional(resp map[string]any) string {
	hso, _ := resp["hookSpecificOutput"].(map[string]any)
	s, _ := hso["additionalContext"].(string)
	return s
}

func hookRaw(w watcher, in string) string {
	var out bytes.Buffer
	w.hook(strings.NewReader(in), &out)
	return out.String()
}

// logLines decodes the rig's log.
func (r *rig) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(r.log.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, l)
		}
		lines = append(lines, m)
	}
	return lines
}

func (r *rig) lastDecision(t *testing.T) string {
	t.Helper()
	lines := r.logLines(t)
	if len(lines) == 0 {
		t.Fatal("nothing logged")
	}
	d, _ := lines[len(lines)-1]["decision"].(string)
	return d
}

func wantContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output lacks %q:\n%s", w, got)
		}
	}
}

func wantSilent(t *testing.T, got string) {
	t.Helper()
	if got != "" {
		t.Fatalf("hook spoke, want silence:\n%s", got)
	}
}

func TestHookUpstreamOverlap(t *testing.T) {
	t.Parallel()
	for _, ev := range []func(string) map[string]any{startEvent, promptEvent} {
		t.Run(ev("")["hook_event_name"].(string), func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			w := r.f.worktree("w", "feat/a")
			r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
			r.f.upstream(map[string]string{"a.go": "package a // up 1\n"})
			r.f.upstream(map[string]string{"b.go": "package b // up 2\n"})
			got := runHook(t, r.w, ev(w))
			wantContains(t, got,
				"driftguard: feat/a overlaps other work.\n",
				"origin/master is 2 commits past your merge base; 1 of them changes 1 file your branch also changes:\n- a.go\n",
				"git rebase origin/master")
			if d := r.lastDecision(t); d != "spoke" {
				t.Errorf("decision = %q, want spoke", d)
			}
		})
	}
}

func TestHookSiblingOverlap(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	w := r.f.worktree("w", "feat/a")
	r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
	x := r.f.worktree("x", "feat/x")
	r.f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
	r.f.push(x, "feat/x")
	got := runHook(t, r.w, startEvent(w))
	wantContains(t, got, "feat/x (pushed) changes 1 file your branch also changes:\n- a.go\n")
	if strings.Contains(got, r.f.dir) {
		t.Fatalf("output names a worktree path:\n%s", got)
	}
}

func TestHookSilentWithoutNews(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	w := r.f.worktree("w", "feat/a")
	r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
	x := r.f.worktree("x", "feat/x")
	r.f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
	wantContains(t, runHook(t, r.w, startEvent(w)), "feat/x (local only)")
	r.clock.advance(31 * time.Minute)
	wantSilent(t, runHook(t, r.w, promptEvent(w)))
	if d := r.lastDecision(t); d != "quiet" {
		t.Errorf("decision = %q, want quiet", d)
	}
	wantContains(t, runHook(t, r.w, startEvent(w)), "feat/x (local only)")
}

func TestHookSpeaksAgainAfterGrowth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(f *fixture, w string)
		grow  func(f *fixture, w string)
		want  string
	}{
		{
			name:  "new upstream commit",
			setup: func(f *fixture, _ string) { f.upstream(map[string]string{"a.go": "package a // up 1\n"}) },
			grow:  func(f *fixture, _ string) { f.upstream(map[string]string{"a.go": "package a // up 2\n"}) },
			want:  "origin/master is 2 commits past your merge base; 2 of them change 1 file",
		},
		{
			name: "sibling touches one more file",
			setup: func(f *fixture, _ string) {
				x := f.worktree("x", "feat/x")
				f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
			},
			grow: func(f *fixture, _ string) {
				f.commit(f.dir+"/x", map[string]string{"b.go": "package b // feat/x\n"}, "x2")
			},
			want: "feat/x (local only) changes 2 files",
		},
		{
			name:  "fresh upstream overlap after merging the default branch",
			setup: func(f *fixture, _ string) { f.upstream(map[string]string{"a.go": "package a // up 1\n"}) },
			grow: func(f *fixture, w string) {
				f.git(w, "merge", "-q", "-X", "ours", "-m", "merge master", "origin/master")
				f.upstream(map[string]string{"a.go": "package a // up 2\n"})
			},
			want: "origin/master is 1 commit past your merge base; 1 of them changes 1 file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			w := r.f.worktree("w", "feat/a")
			r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n", "b.go": "package b // feat/a\n"}, "a")
			tt.setup(r.f, w)
			if got := runHook(t, r.w, promptEvent(w)); got == "" {
				t.Fatal("first prompt was silent")
			}
			tt.grow(r.f, w)
			r.clock.advance(30 * time.Minute)
			wantContains(t, runHook(t, r.w, promptEvent(w)), tt.want)
		})
	}
}

func TestHookDefaultBranchReportsNoOverlap(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.f.write(r.f.main, map[string]string{"a.go": "package a // wip on master\n"})
	r.f.upstream(map[string]string{"a.go": "package a // up\n"})
	x := r.f.worktree("x", "feat/x")
	r.f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
	before := r.f.git(r.f.main, "rev-parse", "refs/remotes/origin/master")
	wantSilent(t, runHook(t, r.w, startEvent(r.f.main)))
	if after := r.f.git(r.f.main, "rev-parse", "refs/remotes/origin/master"); after != before {
		t.Error("the default branch's run fetched")
	}
	if d := r.lastDecision(t); d != "default-branch" {
		t.Errorf("decision = %q, want default-branch", d)
	}
}

func TestHookDetachedIsSilentAndStateless(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	w := r.f.worktree("w", "feat/a")
	r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
	r.f.detach(w)
	wantSilent(t, runHook(t, r.w, startEvent(w)))
	if d := r.lastDecision(t); d != "detached" {
		t.Errorf("decision = %q, want detached", d)
	}
	if entries, err := os.ReadDir(r.w.states.dir); err != nil || len(entries) != 0 {
		t.Fatalf("state dir holds %v, %v; want nothing", entries, err)
	}
}

func TestHookThrottle(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*rig, string) {
		r := newRig(t)
		w := r.f.worktree("w", "feat/a")
		r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
		r.f.upstream(map[string]string{"a.go": "package a // up\n"})
		if got := runHook(t, r.w, promptEvent(w)); got == "" {
			t.Fatal("first prompt was silent")
		}
		x := r.f.worktree("x", "feat/x")
		r.f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
		return r, w
	}
	t.Run("prompt waits a full window", func(t *testing.T) {
		t.Parallel()
		r, w := setup(t)
		r.clock.advance(29 * time.Minute)
		wantSilent(t, runHook(t, r.w, promptEvent(w)))
		if d := r.lastDecision(t); d != "throttled" {
			t.Errorf("decision = %q, want throttled", d)
		}
		r.clock.advance(time.Minute)
		wantContains(t, runHook(t, r.w, promptEvent(w)), "feat/x (local only)")
	})
	t.Run("session start ignores the window", func(t *testing.T) {
		t.Parallel()
		r, w := setup(t)
		r.clock.advance(time.Minute)
		wantContains(t, runHook(t, r.w, startEvent(w)), "feat/x (local only)")
	})
}

func TestHookFetchWindowIsPerRepo(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	w := r.f.worktree("w", "feat/a")
	r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
	r.f.upstream(map[string]string{"a.go": "package a // up 1\n"})
	wantContains(t, runHook(t, r.w, promptEvent(w)), "origin/master is 1 commit past your merge base")

	r.f.upstream(map[string]string{"a.go": "package a // up 2\n"})
	r.clock.advance(time.Minute)
	got := runHook(t, r.w, startEvent(w))
	wantContains(t, got, "origin/master is 1 commit past your merge base")

	v := r.f.worktree("v", "feat/b")
	r.f.commit(v, map[string]string{"a.go": "package a // feat/b\n"}, "b")
	wantContains(t, runHook(t, r.w, startEvent(v)), "origin/master is 1 commit past your merge base")
}

func TestHookFetchFailure(t *testing.T) {
	t.Parallel()
	t.Run("speaks from local refs with a stale note", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		w := r.f.worktree("w", "feat/a")
		r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
		x := r.f.worktree("x", "feat/x")
		r.f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
		r.f.breakOrigin()
		wantContains(t, runHook(t, r.w, promptEvent(w)),
			"driftguard: feat/a overlaps other work. git fetch failed, so origin refs may be stale.\n",
			"feat/x (local only)")
	})
	t.Run("silent without overlap", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		w := r.f.worktree("w", "feat/a")
		r.f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
		r.f.breakOrigin()
		wantSilent(t, runHook(t, r.w, promptEvent(w)))
	})
}

func TestHookBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   func(dir string) string
	}{
		{name: "not JSON", in: func(string) string { return "{not json" }},
		{name: "missing cwd", in: func(string) string { return `{"hook_event_name":"UserPromptSubmit"}` }},
		{name: "cwd outside git", in: func(dir string) string {
			return `{"hook_event_name":"UserPromptSubmit","cwd":"` + dir + `"}`
		}},
		{name: "unknown event", in: func(string) string { return `{"hook_event_name":"Stop","cwd":"/"}` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var log bytes.Buffer
			w := watcher{
				git:    gitRunner{timeout: 10 * time.Second, fetchTimeout: 10 * time.Second},
				now:    (&fakeClock{}).now,
				states: states{dir: t.TempDir()},
				log:    slog.New(slog.NewJSONHandler(&log, nil)),
			}
			if out := hookRaw(w, tt.in(t.TempDir())); out != "" {
				t.Fatalf("hook printed %q", out)
			}
			if n := strings.Count(log.String(), "\n"); n != 1 {
				t.Fatalf("logged %d lines, want 1:\n%s", n, log.String())
			}
		})
	}
}

func TestHookLogsOneLineWithoutThePrompt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	w := r.f.worktree("w", "feat/a")
	runHook(t, r.w, promptEvent(w))
	lines := r.logLines(t)
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1:\n%s", len(lines), r.log.String())
	}
	if _, ok := lines[0]["decision"]; !ok {
		t.Errorf("log line lacks a decision: %v", lines[0])
	}
	if strings.Contains(r.log.String(), "secret prompt text") {
		t.Fatalf("log holds the prompt:\n%s", r.log.String())
	}
}

// post posts an announcement from dir at the rig's current time.
func (r *rig) post(t *testing.T, dir string, args ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := (poster{git: r.f.runner(), now: r.clock.now}).run(dir, append([]string{"post"}, args...), &stdout, &stderr); code != 0 {
		t.Fatalf("post exit %d: %s", code, stderr.String())
	}
}

func TestHookAnnouncementsMatchBranchFiles(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	x := r.f.worktree("x", "feat/x")
	r.f.commit(x, map[string]string{"pkg/m/m.go": "package m // feat/x\n"}, "x")
	r.post(t, x, "--summary", "m.Send takes a context.", "--action", "pass ctx.", "--paths", "pkg/m")
	r.post(t, x, "--summary", "Go 1.27 now.", "--action", "rebuild.")
	sha7 := r.f.git(x, "rev-parse", "--short=7", "HEAD")
	m := r.f.worktree("m", "feat/m")
	r.f.commit(m, map[string]string{"pkg/m/m.go": "package m // feat/m\n"}, "m")
	a := r.f.worktree("a", "feat/a")
	r.f.commit(a, map[string]string{"a.go": "package a // feat/a\n"}, "a")

	wantContains(t, runHook(t, r.w, startEvent(m)),
		"Announcements from other branches:\n",
		"- 2026-03-01 09:00 UTC, feat/x @ "+sha7+": m.Send takes a context. Do: pass ctx. (paths: pkg/m)\n",
		"- 2026-03-01 09:00 UTC, feat/x @ "+sha7+": Go 1.27 now. Do: rebuild.\n")
	got := runHook(t, r.w, startEvent(a))
	wantContains(t, got, "Go 1.27 now.")
	if strings.Contains(got, "m.Send") {
		t.Errorf("a worktree that changes only a.go hears about pkg/m:\n%s", got)
	}
	if got := runHook(t, r.w, startEvent(x)); strings.Contains(got, "Announcements") {
		t.Errorf("the posting branch hears its own announcements:\n%s", got)
	}
}

func TestHookAnnouncementsExpire(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	x := r.f.worktree("x", "feat/x")
	r.post(t, x, "--summary", "old news", "--action", "none")
	a := r.f.worktree("a", "feat/a")
	r.clock.advance(14*24*time.Hour + time.Second)
	wantSilent(t, runHook(t, r.w, startEvent(a)))
}

func TestHookAnnouncementsSeen(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	x := r.f.worktree("x", "feat/x")
	r.post(t, x, "--summary", "heads up", "--action", "adapt")
	a := r.f.worktree("a", "feat/a")
	b := r.f.worktree("b", "feat/b")

	wantContains(t, runHook(t, r.w, promptEvent(a)), "heads up")
	r.clock.advance(31 * time.Minute)
	wantSilent(t, runHook(t, r.w, promptEvent(a)))
	wantContains(t, runHook(t, r.w, startEvent(a)), "heads up")
	wantContains(t, runHook(t, r.w, promptEvent(b)), "heads up")
}

func TestHookAnnouncementsDroppedByTheCapComeBack(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	x := r.f.worktree("x", "feat/x")
	const n = 8
	for i := range n {
		r.post(t, x, "--summary", fmt.Sprintf("note %d %s", i, strings.Repeat("s", 250)), "--action", strings.Repeat("a", 250))
	}
	a := r.f.worktree("a", "feat/a")
	first := runHook(t, r.w, promptEvent(a))
	r.clock.advance(31 * time.Minute)
	second := runHook(t, r.w, promptEvent(a))
	for i := range n {
		note := fmt.Sprintf("note %d ", i)
		if strings.Contains(first, note) == strings.Contains(second, note) {
			t.Errorf("%q in first run %v, in second %v; want exactly one", note, strings.Contains(first, note), strings.Contains(second, note))
		}
	}
	if strings.Contains(first, "note 7 ") {
		t.Fatal("the cap dropped nothing; the test needs more announcements")
	}
}

func TestHookDefaultBranchHearsAnnouncements(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	x := r.f.worktree("x", "feat/x")
	r.post(t, x, "--summary", "m changes", "--action", "adapt", "--paths", "pkg/m")
	r.f.write(r.f.main, map[string]string{"pkg/m/m.go": "package m // wip on master\n"})
	wantContains(t, runHook(t, r.w, startEvent(r.f.main)), "m changes")
}
