package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

const usage = `usage:
  driftguard post --summary S --action A [--paths p1,p2]
`

// poster runs the subcommands a session uses to write to the board.
type poster struct {
	git gitRunner
	now func() time.Time
}

// run executes args in the worktree at cwd and returns the exit status:
// 0 done, 1 refused or failed, 2 usage error.
func (p poster) run(cwd string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return complain(stderr, 2, usage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	switch args[0] {
	case "post":
		return p.post(ctx, cwd, args[1:], stdout, stderr)
	}
	return complain(stderr, 2, usage)
}

func (p poster) post(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	summary := fs.String("summary", "", "what changed")
	action := fs.String("action", "", "what other branches should do")
	list := fs.String("paths", "", "comma-separated paths or patterns it touches")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *summary == "" || *action == "" {
		return complain(stderr, 2, usage)
	}
	r, at, err := p.origin(ctx, cwd)
	if err != nil {
		return fail(stderr, err)
	}
	a, err := newAnnouncement(at, splitPaths(*list), *summary, *action)
	if err != nil {
		return fail(stderr, err)
	}
	b := r.board()
	id, err := b.append(a.line())
	if err != nil {
		return fail(stderr, err)
	}
	if _, err := fmt.Fprintf(stdout, "posted %s to %s\n", id, b.path); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// origin opens the repo at cwd and stamps an entry with its branch, head and
// the current time.
func (p poster) origin(ctx context.Context, cwd string) (repo, posted, error) {
	r, err := openRepo(ctx, p.git, cwd)
	if err != nil {
		return repo{}, posted{}, err
	}
	branch, err := r.branch(ctx)
	if err != nil {
		return repo{}, posted{}, err
	}
	head, err := r.head(ctx)
	if err != nil {
		return repo{}, posted{}, err
	}
	return r, posted{Time: p.now().UTC().Truncate(time.Second), Branch: branch, Head: head}, nil
}

func splitPaths(list string) []string {
	if list == "" {
		return nil
	}
	ps := strings.Split(list, ",")
	for i, s := range ps {
		ps[i] = strings.TrimSpace(s)
	}
	return ps
}

// fail prints err for the session that ran the command and returns 1.
func fail(stderr io.Writer, err error) int {
	var tooLong *lineTooLongError
	switch {
	case errors.As(err, &tooLong):
		return complain(stderr, 1, fmt.Sprintf("entry too long (%d bytes, limit %d): shorten the summary, the action or the paths\n", tooLong.size, maxLine))
	case errors.Is(err, errDetached):
		return complain(stderr, 1, "HEAD is detached: check out the branch the entry is from\n")
	default:
		return complain(stderr, 1, fmt.Sprintf("driftguard: %v\n", err))
	}
}

// complain writes msg to stderr and returns code. A failed write to stderr
// has nowhere left to report to; the exit status still tells.
func complain(stderr io.Writer, code int, msg string) int {
	_, _ = io.WriteString(stderr, msg)
	return code
}
