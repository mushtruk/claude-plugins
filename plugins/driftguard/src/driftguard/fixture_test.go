package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is a repo served by driftguard: a bare origin and its clone main,
// whose worktrees are the branches under test. Every git call runs with an
// empty global config and fixed identities, so results never depend on the
// machine running the tests.
type fixture struct {
	t      *testing.T
	dir    string
	env    []string
	origin string
	main   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:   t,
		dir: dir,
		env: []string{
			"GIT_CONFIG_GLOBAL=" + gitconfig,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Driftguard Test",
			"GIT_AUTHOR_EMAIL=test@driftguard.invalid",
			"GIT_COMMITTER_NAME=Driftguard Test",
			"GIT_COMMITTER_EMAIL=test@driftguard.invalid",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
			"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		},
		origin: filepath.Join(dir, "origin.git"),
		main:   filepath.Join(dir, "main"),
	}
	seed := filepath.Join(dir, "seed")
	f.git(dir, "init", "-q", "-b", "master", seed)
	f.commit(seed, map[string]string{
		"a.go":       "package a\n",
		"b.go":       "package b\n",
		"c.go":       "package c\n",
		"pkg/m/m.go": "package m\n",
	}, "seed")
	f.git(dir, "clone", "-q", "--bare", seed, f.origin)
	f.git(dir, "clone", "-q", f.origin, f.main)
	return f
}

// git runs git in dir and returns its trimmed stdout, failing the test on error.
func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), f.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// runner is the gitRunner under test, sharing the fixture's environment.
func (f *fixture) runner() gitRunner {
	return gitRunner{env: f.env, timeout: 10 * time.Second, fetchTimeout: 10 * time.Second}
}

// worktree adds a worktree of main named name on a new branch off master.
func (f *fixture) worktree(name, branch string) string {
	f.t.Helper()
	dir := filepath.Join(f.dir, name)
	f.git(f.main, "worktree", "add", "-q", "-b", branch, dir, "master")
	return dir
}

// write writes files under dir without committing them.
func (f *fixture) write(dir string, files map[string]string) {
	f.t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

// commit writes files under dir and commits every change in dir.
func (f *fixture) commit(dir string, files map[string]string, msg string) {
	f.t.Helper()
	f.write(dir, files)
	f.git(dir, "add", "-A")
	f.git(dir, "commit", "-q", "-m", msg)
}

func (f *fixture) push(dir, branch string) {
	f.t.Helper()
	f.git(dir, "push", "-q", "origin", branch)
}

// clone makes a fresh clone of origin, standing in for another machine.
func (f *fixture) clone() string {
	f.t.Helper()
	dir, err := os.MkdirTemp(f.dir, "clone-")
	if err != nil {
		f.t.Fatal(err)
	}
	f.git(f.dir, "clone", "-q", f.origin, dir)
	return dir
}

// upstream commits files to master on origin from another clone, leaving
// main's origin refs stale until fetchMain.
func (f *fixture) upstream(files map[string]string) {
	f.t.Helper()
	dir := f.clone()
	f.commit(dir, files, "upstream")
	f.push(dir, "master")
}

func (f *fixture) fetchMain() {
	f.t.Helper()
	f.git(f.main, "fetch", "-q", "origin")
}

func (f *fixture) breakOrigin() {
	f.t.Helper()
	f.git(f.main, "remote", "set-url", "origin", filepath.Join(f.dir, "missing.git"))
}

func (f *fixture) detach(dir string) {
	f.t.Helper()
	f.git(dir, "checkout", "-q", "--detach")
}

// merge lands branch on origin's master through a merge commit, as a pull
// request merge does.
func (f *fixture) merge(branch string) {
	f.t.Helper()
	f.push(f.main, branch)
	dir := f.clone()
	f.git(dir, "merge", "-q", "--no-ff", "-m", "Merge "+branch, "origin/"+branch)
	f.push(dir, "master")
}

func TestFixtureHasOriginHead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if got := f.git(f.main, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); got != "origin/master" {
		t.Fatalf("origin/HEAD = %q, want origin/master", got)
	}
}
