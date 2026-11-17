package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
		{name: "claim without paths", args: []string{"claim", "--summary", "s"}, wantCode: 2, wantStderr: "usage"},
		{name: "claim without summary", args: []string{"claim", "--paths", "pkg/m"}, wantCode: 2, wantStderr: "usage"},
		{name: "release with arguments", args: []string{"release", "pkg/m"}, wantCode: 2, wantStderr: "usage"},
		{name: "claim with a blank path", args: []string{"claim", "--summary", "s", "--paths", " "}, wantCode: 1, wantStderr: "not a clean repo-relative path"},
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

func TestClaimAppendsAClaim(t *testing.T) {
	t.Parallel()
	c := newCLI(t)
	x := c.f.worktree("x", "feat/x")
	code, stdout, stderr := c.run(x, "claim", "--summary", "mailer API", "--paths", "pkg/m, proto/*/mail.proto")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := c.boardLines(t)
	if len(lines) != 1 {
		t.Fatalf("board has %d lines, want 1", len(lines))
	}
	var got line
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Claim == nil || got.Branch != "feat/x" || strings.Join(got.Claim.Paths, ",") != "pkg/m,proto/*/mail.proto" || got.Claim.Summary != "mailer API" {
		t.Errorf("line = %+v, claim %+v", got, got.Claim)
	}
	if want := "claimed " + lineID([]byte(lines[0])) + " to " + c.boardPath() + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestClaimOverlappingAnotherBranchIsRecordedAndNamed(t *testing.T) {
	t.Parallel()
	c := newCLI(t)
	x := c.f.worktree("x", "feat/x")
	y := c.f.worktree("y", "feat/y")
	if code, _, stderr := c.run(x, "claim", "--summary", "mailer API", "--paths", "pkg/m"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := c.run(x, "claim", "--summary", "mailer send", "--paths", "pkg/m/m.go")
	if code != 0 || strings.Count(stdout, "\n") != 1 || !strings.HasPrefix(stdout, "claimed ") {
		t.Errorf("own overlap: exit %d, stdout %q, stderr %q; want only the claimed line", code, stdout, stderr)
	}

	code, stdout, stderr = c.run(y, "claim", "--summary", "mail delivery", "--paths", "pkg/m/m.go")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	out := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(out) != 3 || !strings.HasPrefix(out[0], "claimed ") {
		t.Fatalf("stdout = %q, want the claimed line then one line per overlapping claim", stdout)
	}
	for _, want := range []string{
		"feat/x already claims pkg/m (mailer API). Settle ownership with that branch's session before building; release your claim if it owns the surface.",
		"feat/x already claims pkg/m/m.go (mailer send). Settle ownership with that branch's session before building; release your claim if it owns the surface.",
	} {
		if !slices.Contains(out[1:], want) {
			t.Errorf("stdout %q lacks %q", stdout, want)
		}
	}
	if n := len(c.boardLines(t)); n != 3 {
		t.Errorf("board has %d lines, want 3", n)
	}
}

func TestReleaseCountsTheClaimsItEnds(t *testing.T) {
	t.Parallel()
	c := newCLI(t)
	x := c.f.worktree("x", "feat/x")
	y := c.f.worktree("y", "feat/y")
	for _, args := range [][]string{
		{"claim", "--summary", "one", "--paths", "pkg/m"},
		{"claim", "--summary", "two", "--paths", "a.go"},
	} {
		if code, _, stderr := c.run(x, args...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
	}
	if code, _, stderr := c.run(y, "claim", "--summary", "three", "--paths", "b.go"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"released 2 claims\n", "released 0 claims\n"} {
		if code, stdout, stderr := c.run(x, "release"); code != 0 || stdout != want {
			t.Errorf("release: exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
		}
	}
	if n := len(c.boardLines(t)); n != 5 {
		t.Errorf("board has %d lines, want 5", n)
	}
}
