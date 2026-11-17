package main

import (
	"fmt"
	"regexp"
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

func TestRenderClaimsComeFirst(t *testing.T) {
	t.Parallel()
	anns := []announcement{
		{posted: posted{Time: claimTime, Branch: "feat/y", Head: "bbbbbbb", id: "a1"}, announcementPayload: announcementPayload{Summary: "s1", Action: "a1"}},
		{posted: posted{Time: claimTime, Branch: "feat/y", Head: "bbbbbbb", id: "a2"}, announcementPayload: announcementPayload{Summary: "s2", Action: "a2"}},
	}
	got, shown := render("feat/a", "master", report{
		claims:        []claimFinding{{claim: testClaim("c1", "feat/x", 0, claimTime, "pkg/m"), files: paths{"pkg/m/m.go", "pkg/m/n.go"}}},
		announcements: anns,
		siblings:      []siblingFinding{{branch: "feat/s", files: paths{"a.go"}}},
	})
	claimLine := "feat/x owns pkg/m/m.go, pkg/m/n.go (summary c1). Build on its shape; don't add a parallel one. Coordinate through that branch's session.\n"
	i, j, k := strings.Index(got, claimLine), strings.Index(got, "Announcements from other branches:"), strings.Index(got, "feat/s (local only)")
	if i < 0 || j < 0 || k < 0 || i > j || j > k {
		t.Fatalf("want the claim, then announcements, then siblings:\n%s", got)
	}
	if len(shown) != 2 {
		t.Errorf("shown = %d announcements, want 2", len(shown))
	}
}

func TestRenderClaimNamesUpToEightFiles(t *testing.T) {
	t.Parallel()
	got, _ := render("feat/a", "master", report{
		claims: []claimFinding{{claim: testClaim("c1", "feat/x", 0, claimTime, "pkg"), files: manyFiles(11)}},
	})
	if want := "feat/x owns pkg/f00.go, pkg/f01.go, pkg/f02.go, pkg/f03.go, pkg/f04.go, pkg/f05.go, pkg/f06.go, pkg/f07.go, and 3 more (summary c1)."; !strings.Contains(got, want) {
		t.Errorf("render lacks %q:\n%s", want, got)
	}
}

func TestRenderReturnsOnlyTheAnnouncementsItShows(t *testing.T) {
	t.Parallel()
	var claims []claimFinding
	for i := range 40 {
		claims = append(claims, claimFinding{claim: testClaim(fmt.Sprintf("c%02d", i), "feat/x", i, claimTime, "pkg"), files: manyFiles(8)})
	}
	ann := announcement{posted: posted{Time: claimTime, Branch: "feat/y", Head: "bbbbbbb", id: "a1"}, announcementPayload: announcementPayload{Summary: "s", Action: "a"}}
	got, shown := render("feat/a", "master", report{claims: claims, announcements: []announcement{ann}})
	if strings.Contains(got, "Announcements from other branches:") || len(shown) != 0 {
		t.Fatalf("claims fill the cap, yet shown = %d announcements:\n%s", len(shown), got)
	}
}

// gitCommand is a git command in a notice: its subcommand and first argument.
var gitCommand = regexp.MustCompile(`git (\S+) (\S+)`)

func TestRenderNeverSuggestsChangingAnotherBranch(t *testing.T) {
	t.Parallel()
	full := report{
		claims: []claimFinding{{claim: testClaim("c1", "feat/x", 0, claimTime, "pkg/m"), files: paths{"pkg/m/m.go"}}},
		announcements: []announcement{{
			posted:              posted{Time: claimTime, Branch: "feat/y", Head: "bbbbbbb"},
			announcementPayload: announcementPayload{Paths: patterns{"pkg/m"}, Summary: "Send takes a context", Action: "pass ctx"},
		}},
		upstream: upstreamFinding{commits: 3, shas: []string{"s1"}, files: paths{"a.go"}},
		siblings: []siblingFinding{{branch: "feat/s", pushed: true, files: paths{"a.go"}}, {branch: "feat/t", files: paths{"b.go"}}},
	}
	allowed := map[string]bool{"merge origin/master": true, "rebase origin/master": true, "diff --name-only": true}
	for _, pushed := range []bool{true, false} {
		full.pushed = pushed
		got, _ := render("feat/a", "master", full)
		cmds := gitCommand.FindAllStringSubmatch(got, -1)
		if len(cmds) == 0 {
			t.Fatalf("no git command in:\n%s", got)
		}
		for _, m := range cmds {
			if cmd := m[1] + " " + strings.TrimSuffix(m[2], "."); !allowed[cmd] {
				t.Errorf("render suggests git %s:\n%s", cmd, got)
			}
		}
		for _, banned := range []string{"checkout", "push ", "worktree", "merge feat/", "rebase feat/", "merge origin/feat", "rebase origin/feat"} {
			if strings.Contains(got, banned) {
				t.Errorf("render holds %q:\n%s", banned, got)
			}
		}
	}
}
