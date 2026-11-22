package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGitRunnerExpiredContextRunsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	target := filepath.Join(f.dir, "never")
	if _, err := f.runner().run(ctx, f.dir, "init", "-q", target); err == nil {
		t.Fatal("run with an expired context succeeded")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git ran: stat %s: %v", target, err)
	}
}

func TestGitRunnerErrorNamesSubcommandAndStderr(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rev := strings.Repeat("x", 300)
	_, err := f.runner().run(context.Background(), f.main, "log", rev)
	var ge *gitError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *gitError", err)
	}
	if ge.subcommand != "log" {
		t.Errorf("subcommand = %q, want log", ge.subcommand)
	}
	if len(ge.stderr) != 200 {
		t.Errorf("stderr has %d bytes, want 200: %q", len(ge.stderr), ge.stderr)
	}
	if !strings.Contains(ge.stderr, "xxxx") {
		t.Errorf("stderr %q lacks git's message", ge.stderr)
	}
	if !strings.HasPrefix(err.Error(), "git log: ") {
		t.Errorf("error %q does not name the subcommand", err)
	}
}

func TestGitRunnerFetchUpdatesOriginRef(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	before := f.git(f.main, "rev-parse", "refs/remotes/origin/master")
	f.upstream(map[string]string{"a.go": "package a // upstream\n"})
	if err := f.runner().fetch(context.Background(), f.main, "master"); err != nil {
		t.Fatal(err)
	}
	after := f.git(f.main, "rev-parse", "refs/remotes/origin/master")
	want := f.git(f.origin, "rev-parse", "refs/heads/master")
	if after == before || after != want {
		t.Fatalf("origin/master = %s, want %s (was %s)", after, want, before)
	}
}

func TestGitRunnerFetchBrokenOrigin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.breakOrigin()
	if err := f.runner().fetch(context.Background(), f.main, "master"); err == nil {
		t.Fatal("fetch from a missing origin succeeded")
	}
}

// A fetch that hangs past its timeout stops the whole process group, so no
// transport helper outlives the call.
func TestGitRunnerFetchTimeoutStopsProcessGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fifo := filepath.Join(f.dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(f.dir, "helper.pid")
	script := filepath.Join(f.dir, "helper.sh")
	body := "echo $$ > " + pidFile + "\nread line < " + fifo + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(f.main, "remote", "set-url", "origin", "ext::sh "+script)

	g := f.runner()
	g.env = append(g.env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.ext.allow", "GIT_CONFIG_VALUE_0=always")
	g.fetchTimeout = time.Second
	if err := g.fetch(context.Background(), f.main, "master"); err == nil {
		t.Fatal("hung fetch succeeded")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("helper never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

// waitFor polls cond until it holds, failing the test at the deadline.
func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(limit)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("condition not met within %s", limit)
		case <-tick.C:
		}
	}
}
