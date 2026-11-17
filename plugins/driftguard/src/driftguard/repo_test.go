package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// survey is what the repo reports about one worktree's branch.
type survey struct {
	files, uncommitted paths
	commits            int
	overlapping        int
	upstreamFiles      paths
	siblings           []siblingFinding
	skipped            int
}

func surveyOf(t *testing.T, f *fixture, dir string) survey {
	t.Helper()
	ctx := context.Background()
	r, err := openRepo(ctx, f.runner(), dir)
	if err != nil {
		t.Fatal(err)
	}
	def, err := r.defaultBranch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := r.branch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	uncommitted, err := r.uncommittedFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.branchFiles(ctx, def, uncommitted)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := r.upstream(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := r.pushed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	own := branchWork{branch: branch, files: files, uncommitted: uncommitted}
	sibs, skipped, err := r.siblings(ctx, def, own, pushed)
	if err != nil {
		t.Fatal(err)
	}
	up := upstreamOverlap(commits, files)
	return survey{
		files:         files,
		uncommitted:   uncommitted,
		commits:       up.commits,
		overlapping:   len(up.shas),
		upstreamFiles: up.files,
		siblings:      sibs,
		skipped:       len(skipped),
	}
}

func TestRepoSurvey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(f *fixture) string
		want  survey
	}{
		{
			name: "upstream overlap",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				f.upstream(map[string]string{"a.go": "package a // up 1\n"})
				f.upstream(map[string]string{"b.go": "package b // up 2\n"})
				f.fetchMain()
				return w
			},
			want: survey{files: paths{"a.go"}, commits: 2, overlapping: 1, upstreamFiles: paths{"a.go"}},
		},
		{
			name: "uncommitted, staged and untracked files are branch files",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				f.write(w, map[string]string{"b.go": "package b // edit\n", "c.go": "package c // staged\n", "n.go": "package n\n"})
				f.git(w, "add", "c.go")
				return w
			},
			want: survey{files: paths{"a.go", "b.go", "c.go", "n.go"}, uncommitted: paths{"b.go", "c.go", "n.go"}},
		},
		{
			name: "no overlap",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				f.upstream(map[string]string{"c.go": "package c // up\n"})
				f.fetchMain()
				return w
			},
			want: survey{files: paths{"a.go"}, commits: 1},
		},
		{
			name: "rename counts old and new paths",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.git(w, "mv", "a.go", "z.go")
				f.git(w, "commit", "-q", "-m", "rename")
				return w
			},
			want: survey{files: paths{"a.go", "z.go"}},
		},
		{
			name: "sibling overlap, local only",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n", "b.go": "package b // feat/a\n"}, "a")
				x := f.worktree("x", "feat/x")
				f.commit(x, map[string]string{"a.go": "package a // feat/x\n", "c.go": "package c // feat/x\n"}, "x")
				return w
			},
			want: survey{
				files:    paths{"a.go", "b.go"},
				siblings: []siblingFinding{{branch: "feat/x", files: paths{"a.go"}}},
			},
		},
		{
			name: "sibling overlap, pushed",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				x := f.worktree("x", "feat/x")
				f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
				f.push(x, "feat/x")
				return w
			},
			want: survey{
				files:    paths{"a.go"},
				siblings: []siblingFinding{{branch: "feat/x", pushed: true, files: paths{"a.go"}}},
			},
		},
		{
			name: "sibling's uncommitted edits don't count",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				x := f.worktree("x", "feat/x")
				f.write(x, map[string]string{"a.go": "package a // feat/x\n"})
				return w
			},
			want: survey{files: paths{"a.go"}},
		},
		{
			name: "default branch with local commits is no sibling",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				f.commit(f.main, map[string]string{"a.go": "package a // local master\n"}, "m")
				return w
			},
			want: survey{files: paths{"a.go"}},
		},
		{
			name: "detached sibling",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				d := f.worktree("d", "feat/d")
				f.commit(d, map[string]string{"a.go": "package a // feat/d\n"}, "d")
				f.detach(d)
				return w
			},
			want: survey{files: paths{"a.go"}},
		},
		{
			name: "sibling merged into origin",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				m := f.worktree("m", "feat/m")
				f.commit(m, map[string]string{"a.go": "package a // feat/m\n"}, "m")
				f.merge("feat/m")
				f.fetchMain()
				return w
			},
			want: survey{files: paths{"a.go"}, commits: 2, overlapping: 1, upstreamFiles: paths{"a.go"}},
		},
		{
			name: "prunable sibling is read from its branch ref",
			setup: func(f *fixture) string {
				w := f.worktree("w", "feat/a")
				f.commit(w, map[string]string{"a.go": "package a // feat/a\n"}, "a")
				p := f.worktree("p", "feat/p")
				f.commit(p, map[string]string{"a.go": "package a // feat/p\n"}, "p")
				if err := os.RemoveAll(p); err != nil {
					f.t.Fatal(err)
				}
				return w
			},
			want: survey{
				files:    paths{"a.go"},
				siblings: []siblingFinding{{branch: "feat/p", files: paths{"a.go"}}},
			},
		},
		{
			name: "stacked branch ignores files inherited from its base",
			setup: func(f *fixture) string {
				x := f.worktree("x", "feat/x")
				f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
				y := f.worktree("y", "feat/y")
				f.git(y, "reset", "-q", "--hard", "feat/x")
				f.commit(y, map[string]string{"b.go": "package b // feat/y\n"}, "y")
				return y
			},
			want: survey{files: paths{"a.go", "b.go"}},
		},
		{
			name: "stacked branch hears its base's new overlap",
			setup: func(f *fixture) string {
				x := f.worktree("x", "feat/x")
				f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
				y := f.worktree("y", "feat/y")
				f.git(y, "reset", "-q", "--hard", "feat/x")
				f.commit(y, map[string]string{"b.go": "package b // feat/y\n"}, "y")
				f.commit(x, map[string]string{"b.go": "package b // feat/x\n"}, "x2")
				return y
			},
			want: survey{
				files:    paths{"a.go", "b.go"},
				siblings: []siblingFinding{{branch: "feat/x", files: paths{"b.go"}}},
			},
		},
		{
			name: "base branch hears its stacked branch only past the fork",
			setup: func(f *fixture) string {
				x := f.worktree("x", "feat/x")
				f.commit(x, map[string]string{"a.go": "package a // feat/x\n"}, "x")
				y := f.worktree("y", "feat/y")
				f.git(y, "reset", "-q", "--hard", "feat/x")
				f.commit(y, map[string]string{"b.go": "package b // feat/y\n"}, "y")
				f.write(x, map[string]string{"b.go": "package b // feat/x wip\n"})
				return x
			},
			want: survey{
				files:       paths{"a.go", "b.go"},
				uncommitted: paths{"b.go"},
				siblings:    []siblingFinding{{branch: "feat/y", files: paths{"b.go"}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			dir := tt.setup(f)
			if got := surveyOf(t, f, dir); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("survey =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestRepoBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worktree("w", "feat/a")
	ctx := context.Background()
	r, err := openRepo(ctx, f.runner(), w)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.branch(ctx); err != nil || got != "feat/a" {
		t.Fatalf("branch = %q, %v; want feat/a", got, err)
	}
	if r.root != w {
		t.Errorf("root = %q, want %q", r.root, w)
	}
	if want := f.main + "/.git"; r.commonDir != want {
		t.Errorf("commonDir = %q, want %q", r.commonDir, want)
	}
	f.detach(w)
	if _, err := r.branch(ctx); !errors.Is(err, errDetached) {
		t.Fatalf("branch on detached HEAD: err = %v, want errDetached", err)
	}
}

func TestRepoDefaultBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	r, err := openRepo(ctx, f.runner(), f.main)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.defaultBranch(ctx); err != nil || got != "master" {
		t.Fatalf("defaultBranch = %q, %v; want master", got, err)
	}
	f.git(f.main, "remote", "set-head", "origin", "-d")
	if _, err := r.defaultBranch(ctx); !errors.Is(err, errNoOriginHead) {
		t.Fatalf("defaultBranch without origin/HEAD: err = %v, want errNoOriginHead", err)
	}
}

// landingOf opens dir and returns landing's report on branches.
func landingOf(t *testing.T, f *fixture, dir string, branches ...string) map[string]tip {
	t.Helper()
	ctx := context.Background()
	r, err := openRepo(ctx, f.runner(), dir)
	if err != nil {
		t.Fatal(err)
	}
	tips, err := r.landing(ctx, "master", branches)
	if err != nil {
		t.Fatal(err)
	}
	return tips
}

func TestRepoLandingFollowsABranchUntilItLands(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	wa := f.worktree("wa", "feat/a")
	f.commit(wa, map[string]string{"a.go": "package a // a\n"}, "a")
	wy := f.worktree("wy", "feat/y")

	tips := landingOf(t, f, f.main, "feat/a", "feat/y")
	if got := tips["feat/a"]; got.sha != f.git(wa, "rev-parse", "HEAD") || got.merged {
		t.Errorf("local-only branch: tip = %+v, want its head unmerged", got)
	}
	if got, ok := tips["feat/y"]; !ok || got.merged {
		t.Errorf("branch at origin/master's tip: tip = %+v, %v; want present, unmerged", got, ok)
	}

	f.upstream(map[string]string{"c.go": "package c // up\n"})
	f.fetchMain()
	f.git(wy, "merge", "-q", "--ff-only", "origin/master")
	if got := landingOf(t, f, f.main, "feat/y")["feat/y"]; got.merged || got.sha != f.git(f.main, "rev-parse", "origin/master") {
		t.Errorf("fast-forwarded to origin/master: tip = %+v, want origin/master's tip unmerged", got)
	}

	f.commit(wy, map[string]string{"b.go": "package b // y\n"}, "y")
	f.merge("feat/y")
	f.fetchMain()
	if got := landingOf(t, f, f.main, "feat/y")["feat/y"]; !got.merged {
		t.Errorf("after its merge: tip = %+v, want merged", got)
	}

	f.detach(wy)
	f.deleteBranch("feat/y")
	if got, ok := landingOf(t, f, f.main, "feat/y")["feat/y"]; ok {
		t.Errorf("after deletion: tip = %+v, want absent", got)
	}
}

func TestRepoLandingCreated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	created := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	w := f.at(created.Format(time.RFC3339)).worktree("w", "feat/r")
	f.at("2026-03-01T00:00:00Z").commit(w, map[string]string{"a.go": "package a // 1\n"}, "one")
	f.at("2026-04-01T00:00:00Z").commit(w, map[string]string{"a.go": "package a // 2\n"}, "two")
	f.git(f.main, "-c", "core.logAllRefUpdates=false", "branch", "feat/n", "master")
	other := f.clone()
	f.git(other, "checkout", "-q", "-b", "feat/o")
	f.commit(other, map[string]string{"o.go": "package o\n"}, "o")
	f.push(other, "feat/o")
	f.fetchMain()

	tips := landingOf(t, f, f.main, "feat/r", "feat/n", "feat/o")
	if got := tips["feat/r"].created; !got.Equal(created) {
		t.Errorf("created = %v, want the branch's creation %v", got, created)
	}
	if got, ok := tips["feat/n"]; !ok || !got.created.IsZero() {
		t.Errorf("branch without a reflog: tip = %+v, %v; want present with zero created", got, ok)
	}
	if got, ok := tips["feat/o"]; !ok || !got.created.IsZero() || got.sha != f.git(other, "rev-parse", "HEAD") {
		t.Errorf("branch only on origin: tip = %+v, %v; want origin's tip with zero created", got, ok)
	}
}
