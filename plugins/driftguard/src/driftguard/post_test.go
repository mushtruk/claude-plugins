package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cli runs a subcommand in dir at the fake clock's time.
type cli struct {
	f     *fixture
	clock *fakeClock
}

func newCLI(t *testing.T) cli {
	t.Helper()
	return cli{f: newFixture(t), clock: &fakeClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}}
}

func (c cli) run(dir string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = poster{git: c.f.runner(), now: c.clock.now}.run(dir, args, &out, &errb)
	return code, out.String(), errb.String()
}

func (c cli) boardPath() string {
	return filepath.Join(c.f.main, ".git", "driftguard", "board.jsonl")
}

func (c cli) boardLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(c.boardPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestPostAppendsAnAnnouncement(t *testing.T) {
	t.Parallel()
	c := newCLI(t)
	x := c.f.worktree("x", "feat/x")
	code, stdout, stderr := c.run(x, "post", "--summary", "Send takes a context", "--action", "pass ctx", "--paths", "pkg/m,a.go")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := c.boardLines(t)
	if len(lines) != 1 {
		t.Fatalf("board has %d lines, want 1", len(lines))
	}
	var got struct {
		Time         time.Time `json:"time"`
		Branch       string    `json:"branch"`
		Head         string    `json:"head"`
		Announcement struct {
			Paths   []string `json:"paths"`
			Summary string   `json:"summary"`
			Action  string   `json:"action"`
		} `json:"announcement"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(c.clock.t) || got.Branch != "feat/x" || got.Head != c.f.git(x, "rev-parse", "HEAD") {
		t.Errorf("envelope = %v %q %q", got.Time, got.Branch, got.Head)
	}
	a := got.Announcement
	if strings.Join(a.Paths, ",") != "pkg/m,a.go" || a.Summary != "Send takes a context" || a.Action != "pass ctx" {
		t.Errorf("announcement = %+v", a)
	}
	if want := "posted " + lineID([]byte(lines[0])) + " to " + c.boardPath() + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestPostFromTwoWorktreesSharesOneBoard(t *testing.T) {
	t.Parallel()
	c := newCLI(t)
	for _, wt := range []struct{ name, branch string }{{"x", "feat/x"}, {"y", "feat/y"}} {
		dir := c.f.worktree(wt.name, wt.branch)
		if code, _, stderr := c.run(dir, "post", "--summary", "s", "--action", "a"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
	}
	if n := len(c.boardLines(t)); n != 2 {
		t.Fatalf("board has %d lines, want 2", n)
	}
}

func TestPostRefuses(t *testing.T) {
	t.Parallel()
	wide := strings.Repeat("\U0001D11E", 300)
	tests := []struct {
		name       string
		detach     bool
		args       []string
		wantCode   int
		wantStderr string
	}{
		{
			name:       "entry too long",
			args:       []string{"post", "--summary", wide, "--action", wide, "--paths", strings.Join(longPaths(10, 200), ",")},
			wantCode:   1,
			wantStderr: "bytes, limit 4096): shorten the summary, the action or the paths\n",
		},
		{name: "detached HEAD", detach: true, args: []string{"post", "--summary", "s", "--action", "a"}, wantCode: 1, wantStderr: "detached"},
		{name: "missing summary", args: []string{"post", "--action", "a"}, wantCode: 2, wantStderr: "usage"},
		{name: "no subcommand", args: nil, wantCode: 2, wantStderr: "usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCLI(t)
			x := c.f.worktree("x", "feat/x")
			if tt.detach {
				c.f.detach(x)
			}
			code, stdout, stderr := c.run(x, tt.args...)
			if code != tt.wantCode || !strings.Contains(stderr, tt.wantStderr) || stdout != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d, stderr with %q", code, stdout, stderr, tt.wantCode, tt.wantStderr)
			}
			if n := len(c.boardLines(t)); n != 0 {
				t.Errorf("board has %d lines, want none", n)
			}
		})
	}
}
