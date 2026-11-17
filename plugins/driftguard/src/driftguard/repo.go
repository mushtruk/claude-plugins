package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	errDetached     = errors.New("HEAD is detached")
	errNoOriginHead = errors.New("refs/remotes/origin/HEAD is not set")
)

// repo is one worktree of a git repository, read through git only.
type repo struct {
	git       gitRunner
	root      string
	commonDir string
}

// branchWork is what this worktree's branch changes against the default
// branch.
type branchWork struct {
	branch      string
	files       paths // committed since the merge base, plus uncommitted
	uncommitted paths
}

func openRepo(ctx context.Context, g gitRunner, cwd string) (repo, error) {
	out, err := g.run(ctx, cwd, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return repo{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return repo{}, fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	return repo{git: g, root: lines[0], commonDir: lines[1]}, nil
}

func (r repo) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.git.run(ctx, r.root, args...)
}

func (r repo) line(ctx context.Context, args ...string) (string, error) {
	out, err := r.run(ctx, args...)
	return strings.TrimSpace(string(out)), err
}

func (r repo) nulPaths(ctx context.Context, args ...string) (paths, error) {
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return newPaths(splitNul(out)), nil
}

// branch returns the checked-out branch, or errDetached.
func (r repo) branch(ctx context.Context) (string, error) {
	b, err := r.line(ctx, "symbolic-ref", "-q", "--short", "HEAD")
	if exitStatus(err) == 1 {
		return "", errDetached
	}
	return b, err
}

// defaultBranch returns the branch origin/HEAD points at, or errNoOriginHead.
func (r repo) defaultBranch(ctx context.Context) (string, error) {
	ref, err := r.line(ctx, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD")
	if exitStatus(err) == 1 {
		return "", errNoOriginHead
	}
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(ref, "origin/"), nil
}

// uncommittedFiles returns modified, staged, deleted and untracked files that
// are not ignored.
func (r repo) uncommittedFiles(ctx context.Context) (paths, error) {
	out, err := r.run(ctx, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range splitNul(out) {
		// Each entry is "XY <path>".
		if len(entry) > 3 {
			files = append(files, entry[3:])
		}
	}
	return newPaths(files), nil
}

// branchFiles returns the files this branch's commits change since its merge
// base with the default branch, plus the given uncommitted files.
func (r repo) branchFiles(ctx context.Context, def string, uncommitted paths) (paths, error) {
	committed, err := r.nulPaths(ctx, "diff", "--name-only", "--no-renames", "-z", "origin/"+def+"...HEAD")
	if err != nil {
		return nil, err
	}
	return committed.union(uncommitted), nil
}

// upstream returns the commits on origin/<def> since this branch's merge base.
func (r repo) upstream(ctx context.Context, def string) ([]commit, error) {
	out, err := r.run(ctx, "log", "--no-renames", "--name-only", "-z", "--format=%x00%H", "origin/"+def, "^HEAD")
	if err != nil {
		return nil, err
	}
	// Each commit reads "\x00<sha>\x00\n<file>\x00<file>\x00...", so a sha is
	// the token after an empty one.
	var commits []commit
	var files []string
	afterEmpty := false
	for _, tok := range bytes.Split(out, []byte{0}) {
		name := strings.TrimPrefix(string(tok), "\n")
		switch {
		case name == "":
			afterEmpty = true
			continue
		case afterEmpty:
			if len(commits) > 0 {
				commits[len(commits)-1].files = newPaths(files)
			}
			commits = append(commits, commit{sha: name})
			files = nil
		default:
			files = append(files, name)
		}
		afterEmpty = false
	}
	if len(commits) > 0 {
		commits[len(commits)-1].files = newPaths(files)
	}
	return commits, nil
}

// worktreeBranches returns the branch checked out in each worktree of the
// repo, skipping detached and bare ones.
func (r repo) worktreeBranches(ctx context.Context) ([]string, error) {
	out, err := r.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, field := range splitNul(out) {
		if b, ok := strings.CutPrefix(field, "branch refs/heads/"); ok {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

// ownCommits returns the commits on this branch that are not on origin/<def>.
func (r repo) ownCommits(ctx context.Context, def string) (map[string]bool, error) {
	out, err := r.run(ctx, "rev-list", "origin/"+def+"..HEAD")
	if err != nil {
		return nil, err
	}
	own := make(map[string]bool)
	for _, sha := range strings.Fields(string(out)) {
		own[sha] = true
	}
	return own, nil
}

func (r repo) mergeBase(ctx context.Context, ref string) (string, error) {
	return r.line(ctx, "merge-base", "HEAD", ref)
}

// siblingFiles returns the files branch's commits change since its merge base
// with the default branch.
func (r repo) siblingFiles(ctx context.Context, def, branch string) (paths, error) {
	return r.nulPaths(ctx, "diff", "--name-only", "--no-renames", "-z", "origin/"+def+"...refs/heads/"+branch)
}

// filesSince returns the files that differ between base and ref.
func (r repo) filesSince(ctx context.Context, base, ref string) (paths, error) {
	return r.nulPaths(ctx, "diff", "--name-only", "--no-renames", "-z", base, ref)
}

// pushed returns the branch names that exist on origin.
func (r repo) pushed(ctx context.Context) (map[string]bool, error) {
	out, err := r.run(ctx, "for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/")
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for _, b := range strings.Fields(string(out)) {
		set[b] = true
	}
	return set, nil
}

// siblings returns, for every other branch checked out in a worktree, the
// committed files it shares with own. A sibling whose files can't be read is
// left out and returned in skipped, keyed by branch.
func (r repo) siblings(ctx context.Context, def string, own branchWork, pushed map[string]bool) (found []siblingFinding, skipped map[string]error, err error) {
	branches, err := r.worktreeBranches(ctx)
	if err != nil {
		return nil, nil, err
	}
	ownCommits, err := r.ownCommits(ctx, def)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range branches {
		if b == own.branch || b == def {
			continue
		}
		files, err := r.siblingOverlap(ctx, def, b, own, ownCommits)
		if err != nil {
			if skipped == nil {
				skipped = make(map[string]error)
			}
			skipped[b] = err
			continue
		}
		if len(files) > 0 {
			found = append(found, siblingFinding{branch: b, pushed: pushed[b], files: files})
		}
	}
	return found, skipped, nil
}

// siblingOverlap compares a sibling stacked on this branch, or this branch's
// base, from their fork point, so commits both inherit don't count.
func (r repo) siblingOverlap(ctx context.Context, def, branch string, own branchWork, ownCommits map[string]bool) (paths, error) {
	ref := "refs/heads/" + branch
	if len(ownCommits) > 0 {
		base, err := r.mergeBase(ctx, ref)
		if err != nil {
			return nil, err
		}
		if ownCommits[base] {
			theirs, err := r.filesSince(ctx, base, ref)
			if err != nil {
				return nil, err
			}
			mine, err := r.filesSince(ctx, base, "HEAD")
			if err != nil {
				return nil, err
			}
			return theirs.intersect(mine.union(own.uncommitted)), nil
		}
	}
	theirs, err := r.siblingFiles(ctx, def, branch)
	if err != nil {
		return nil, err
	}
	return theirs.intersect(own.files), nil
}

func splitNul(out []byte) []string {
	var fields []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			fields = append(fields, string(f))
		}
	}
	return fields
}
