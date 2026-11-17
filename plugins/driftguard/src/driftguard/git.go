package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	// stderrMax bounds the git stderr an error carries.
	stderrMax = 200
	// killDelay is how long a cancelled git gets after SIGTERM to remove its
	// lockfiles before it is killed.
	killDelay = 500 * time.Millisecond
)

// gitRunner runs git with no terminal, so neither git nor ssh can prompt, and
// stops the whole process group when a call outlives its timeout.
type gitRunner struct {
	env          []string // added to the inherited environment
	timeout      time.Duration
	fetchTimeout time.Duration
}

// gitError is a failed git call: its subcommand and the start of its stderr.
type gitError struct {
	subcommand string
	stderr     string
	err        error
}

func (e *gitError) Error() string {
	if e.stderr == "" {
		return fmt.Sprintf("git %s: %v", e.subcommand, e.err)
	}
	return fmt.Sprintf("git %s: %v: %s", e.subcommand, e.err, e.stderr)
}

func (e *gitError) Unwrap() error { return e.err }

// exitStatus returns the exit status of a git call that ran and failed, else -1.
func exitStatus(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// run runs a local git subcommand in dir and returns its stdout.
func (g gitRunner) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return g.exec(ctx, g.timeout, dir, args)
}

// fetch updates origin's branch ref without touching the shared FETCH_HEAD and
// without starting background maintenance, which would outlive the call.
func (g gitRunner) fetch(ctx context.Context, dir, branch string) error {
	_, err := g.exec(ctx, g.fetchTimeout, dir, []string{
		"fetch", "--quiet", "--no-tags", "--no-auto-maintenance", "--no-write-fetch-head",
		"origin", "+refs/heads/" + branch + ":refs/remotes/origin/" + branch,
	})
	return err
}

func (g gitRunner) exec(ctx context.Context, timeout time.Duration, dir string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(cmd.Environ(), g.env...), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// SIGTERM lets git remove its lockfiles; a SIGKILLed fetch leaves a ref
	// lock behind that fails every later fetch.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = killDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cmd.Process != nil && ctx.Err() != nil {
		// Whatever survived SIGTERM in the group (an ssh child) goes too.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > stderrMax {
			msg = msg[:stderrMax]
		}
		return nil, &gitError{subcommand: args[0], stderr: msg, err: err}
	}
	return stdout.Bytes(), nil
}
