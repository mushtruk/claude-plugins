package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// board is the repo's board, shared by every worktree.
func (r repo) board() board {
	return board{path: filepath.Join(r.commonDir, "driftguard", "board.jsonl")}
}

// head returns the full sha of HEAD.
func (r repo) head(ctx context.Context) (string, error) {
	return r.line(ctx, "rev-parse", "HEAD")
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

// set returns the whitespace-separated words of a git call's output.
func (r repo) set(ctx context.Context, args ...string) (map[string]bool, error) {
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	words := make(map[string]bool)
	for _, w := range strings.Fields(string(out)) {
		words[w] = true
	}
	return words, nil
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

// pushed returns the branch names that exist on origin.
func (r repo) pushed(ctx context.Context) (map[string]bool, error) {
	return r.set(ctx, "for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/")
}

// siblings returns, for every other branch checked out in a worktree, the
// committed files it shares with own. A sibling whose files can't be read is
// left out and returned in skipped, keyed by branch.
func (r repo) siblings(ctx context.Context, def string, own branchWork, pushed map[string]bool) (found []siblingFinding, skipped map[string]error, err error) {
	branches, err := r.worktreeBranches(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range branches {
		if b == own.branch || b == def {
			continue
		}
		files, err := r.siblingOverlap(ctx, def, b, own)
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

// siblingOverlap compares only the commits each side has and the other and
// origin/<def> lack, so commits both inherit, from a stacked base or from
// merging the default branch, don't count.
func (r repo) siblingOverlap(ctx context.Context, def, branch string, own branchWork) (paths, error) {
	ref := "refs/heads/" + branch
	theirs, err := r.commitFiles(ctx, "origin/"+def+".."+ref, "^HEAD")
	if err != nil {
		return nil, err
	}
	mine, err := r.commitFiles(ctx, "origin/"+def+"..HEAD", "^"+ref)
	if err != nil {
		return nil, err
	}
	return theirs.intersect(mine.union(own.uncommitted)), nil
}

// commitFiles returns the files the non-merge commits in revs change.
func (r repo) commitFiles(ctx context.Context, revs ...string) (paths, error) {
	return r.nulPaths(ctx, append([]string{"log", "--no-renames", "--name-only", "-z", "--format="}, revs...)...)
}

// tip is what git knows of a branch's ref when deciding whether it landed.
type tip struct {
	sha string
	// merged: the tip reached origin/<def> through a merge, off its
	// first-parent chain. A tip on that chain only caught up with it.
	merged bool
	// created is the oldest entry of the local ref's reflog, zero when
	// there is none.
	created time.Time
}

// landing returns the tips of branches, from refs/heads/ or else
// refs/remotes/origin/; a branch with neither ref is absent.
func (r repo) landing(ctx context.Context, def string, branches []string) (map[string]tip, error) {
	if len(branches) == 0 {
		return nil, nil
	}
	local, remote, err := r.branchRefs(ctx)
	if err != nil {
		return nil, err
	}
	tips := make(map[string]tip)
	ancestors := make(map[string]bool)
	for _, b := range branches {
		sha, ok := local[b]
		if !ok {
			sha, ok = remote[b]
		}
		if _, done := tips[b]; done || !ok {
			continue
		}
		t := tip{sha: sha}
		ancestor, err := r.isAncestor(ctx, sha, "origin/"+def)
		if err != nil {
			return nil, err
		}
		if ancestor {
			ancestors[b] = true
		}
		if _, ok := local[b]; ok {
			if t.created, err = r.created(ctx, b); err != nil {
				return nil, err
			}
		}
		tips[b] = t
	}
	if len(ancestors) == 0 {
		return tips, nil
	}
	chain, err := r.firstParents(ctx, "origin/"+def)
	if err != nil {
		return nil, err
	}
	for b := range ancestors {
		t := tips[b]
		t.merged = !chain[t.sha]
		tips[b] = t
	}
	return tips, nil
}

// liveClaims returns the claims in e still in force.
func (r repo) liveClaims(ctx context.Context, def string, e entries) ([]claim, error) {
	tips, err := r.landing(ctx, def, e.claimants())
	if err != nil {
		return nil, err
	}
	return e.live(tips), nil
}

// branchRefs returns the tip of every local branch and every origin branch.
func (r repo) branchRefs(ctx context.Context) (local, remote map[string]string, err error) {
	out, err := r.run(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/remotes/origin/")
	if err != nil {
		return nil, nil, err
	}
	local, remote = make(map[string]string), make(map[string]string)
	for l := range strings.Lines(string(out)) {
		ref, sha, _ := strings.Cut(strings.TrimSpace(l), " ")
		if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			local[b] = sha
		} else if b, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok {
			remote[b] = sha
		}
	}
	return local, remote, nil
}

func (r repo) isAncestor(ctx context.Context, commit, of string) (bool, error) {
	_, err := r.run(ctx, "merge-base", "--is-ancestor", commit, of)
	if exitStatus(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

// firstParents returns the commits on ref's first-parent chain.
func (r repo) firstParents(ctx context.Context, ref string) (map[string]bool, error) {
	return r.set(ctx, "rev-list", "--first-parent", ref)
}

// created returns the time of the oldest entry in branch's reflog, zero when
// the reflog is empty or missing. Each line reads <branch>@{<unix time>},
// newest first.
func (r repo) created(ctx context.Context, branch string) (time.Time, error) {
	out, err := r.run(ctx, "log", "-g", "--date=unix", "--format=%gd", "refs/heads/"+branch)
	if err != nil {
		return time.Time{}, err
	}
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		return time.Time{}, nil
	}
	oldest := lines[len(lines)-1]
	i := strings.LastIndex(oldest, "@{")
	if i < 0 || !strings.HasSuffix(oldest, "}") {
		return time.Time{}, fmt.Errorf("git log -g: unexpected reflog selector %q", oldest)
	}
	secs, err := strconv.ParseInt(oldest[i+2:len(oldest)-1], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("git log -g: unexpected reflog selector %q: %w", oldest, err)
	}
	return time.Unix(secs, 0).UTC(), nil
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
