package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func manyFiles(n int) paths {
	var ps []string
	for i := range n {
		ps = append(ps, fmt.Sprintf("pkg/f%02d.go", i))
	}
	return newPaths(ps)
}

func TestRenderCapsFilesPerSection(t *testing.T) {
	t.Parallel()
	got, _ := render("feat/a", "master", report{
		upstream: upstreamFinding{commits: 40, shas: []string{"s1"}, files: manyFiles(30)},
	})
	for _, want := range []string{
		"origin/master is 40 commits past your merge base; 1 of them changes 30 files your branch also changes:",
		"- pkg/f07.go\n- and 22 more\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pkg/f08.go") {
		t.Errorf("render lists a ninth file:\n%s", got)
	}
}

func TestRenderDropsWholeSectionsOverTheCap(t *testing.T) {
	t.Parallel()
	var sibs []siblingFinding
	for i := range 40 {
		sibs = append(sibs, siblingFinding{branch: fmt.Sprintf("feat/s%02d", i), files: manyFiles(10)})
	}
	got, _ := render("feat/a", "master", report{siblings: sibs})
	if n := utf8.RuneCountInString(got); n > 3000 {
		t.Fatalf("render is %d characters, over 3000", n)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	last := lines[len(lines)-1]
	shown := strings.Count(got, "See all: ")
	if want := fmt.Sprintf("and %d more sections over the 3000-character cap", 40-shown); last != want {
		t.Fatalf("last line = %q, want %q", last, want)
	}
	if prev := lines[len(lines)-2]; !strings.HasPrefix(prev, "See all: ") {
		t.Fatalf("a section was cut mid-way: line before the count is %q", prev)
	}
	if !strings.Contains(got, "feat/s00 (local only)") || strings.Contains(got, "feat/s39") {
		t.Fatalf("sections not dropped from the end:\n%s", got)
	}
}

func TestRenderOrdersSiblingsByOverlapThenName(t *testing.T) {
	t.Parallel()
	got, _ := render("feat/a", "master", report{siblings: []siblingFinding{
		{branch: "feat/b", files: paths{"a.go"}},
		{branch: "feat/c", files: paths{"a.go", "b.go"}},
		{branch: "feat/a2", files: paths{"a.go"}},
	}})
	c, a2, b := strings.Index(got, "feat/c ("), strings.Index(got, "feat/a2 ("), strings.Index(got, "feat/b (")
	if c < 0 || a2 < 0 || b < 0 || c >= a2 || a2 >= b {
		t.Fatalf("siblings out of order:\n%s", got)
	}
}

func TestRenderStaleNoteOnlyWithASection(t *testing.T) {
	t.Parallel()
	if got, _ := render("feat/a", "master", report{stale: true}); got != "" {
		t.Fatalf("stale note alone rendered %q", got)
	}
	got, _ := render("feat/a", "master", report{stale: true, siblings: []siblingFinding{{branch: "feat/x", files: paths{"a.go"}}}})
	if !strings.HasPrefix(got, "driftguard: feat/a overlaps other work. git fetch failed, so origin refs may be stale.\n") {
		t.Fatalf("stale note missing:\n%s", got)
	}
}

func TestRenderRecommendation(t *testing.T) {
	t.Parallel()
	up := upstreamFinding{commits: 1, shas: []string{"s1"}, files: paths{"a.go"}}
	tests := []struct {
		name   string
		pushed bool
		want   string
	}{
		{name: "pushed branch merges", pushed: true, want: "git merge origin/master"},
		{name: "unpushed branch rebases", want: "git rebase origin/master"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := render("feat/a", "master", report{upstream: up, pushed: tt.pushed})
			if !strings.Contains(got, tt.want) {
				t.Fatalf("render lacks %q:\n%s", tt.want, got)
			}
		})
	}
}

func TestRenderSibling(t *testing.T) {
	t.Parallel()
	got, _ := render("feat/a", "master", report{siblings: []siblingFinding{{branch: "feat/x", pushed: true, files: paths{"a.go"}}}})
	want := "driftguard: feat/a overlaps other work.\n" +
		"feat/x (pushed) changes 1 file your branch also changes:\n" +
		"- a.go\n" +
		"See all: git diff --name-only origin/master...feat/x. Adapt your branch, or contact that branch's session if you both rework the same code.\n"
	if got != want {
		t.Fatalf("render =\n%s\nwant\n%s", got, want)
	}
}
