package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// maxFiles names the area of an overlap; the full list is one git
	// command away.
	maxFiles = 8
	// maxChars keeps the notice to about a third of what a context handoff
	// may spend, since it is advice landing in the main context.
	maxChars = 3000
)

// render is the notice for r, or "" when r has no section to show. Every
// instruction it gives is an action inside branch itself.
func render(branch, def string, r report) string {
	var sections []string
	if len(r.upstream.shas) > 0 {
		sections = append(sections, upstreamSection(def, r.upstream, r.pushed))
	}
	sibs := slices.Clone(r.siblings)
	slices.SortFunc(sibs, func(a, b siblingFinding) int {
		return cmp.Or(cmp.Compare(len(b.files), len(a.files)), cmp.Compare(a.branch, b.branch))
	})
	for _, s := range sibs {
		sections = append(sections, siblingSection(def, s))
	}
	if len(sections) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("driftguard: " + branch + " overlaps other work.")
	if r.stale {
		b.WriteString(" git fetch failed, so origin refs may be stale.")
	}
	b.WriteString("\n")
	used := utf8.RuneCountInString(b.String())
	for i, sec := range sections {
		need := used + utf8.RuneCountInString(sec)
		if rest := len(sections) - i - 1; rest > 0 {
			need += utf8.RuneCountInString(omitted(rest))
		}
		if need > maxChars {
			b.WriteString(omitted(len(sections) - i))
			break
		}
		b.WriteString(sec)
		used += utf8.RuneCountInString(sec)
	}
	return b.String()
}

func omitted(n int) string {
	return fmt.Sprintf("and %d more %s over the %d-character cap\n", n, plural(n, "section", "sections"), maxChars)
}

func upstreamSection(def string, u upstreamFinding, pushed bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "origin/%s is %d %s past your merge base; %d of them %s %d %s your branch also changes:\n",
		def, u.commits, plural(u.commits, "commit", "commits"),
		len(u.shas), plural(len(u.shas), "changes", "change"),
		len(u.files), plural(len(u.files), "file", "files"))
	writeFiles(&b, u.files)
	if pushed {
		fmt.Fprintf(&b, "Merge it now while the overlap is small: git merge origin/%s\n", def)
	} else {
		fmt.Fprintf(&b, "Rebase onto it now while the overlap is small: git rebase origin/%s\n", def)
	}
	return b.String()
}

func siblingSection(def string, s siblingFinding) string {
	where := "local only"
	if s.pushed {
		where = "pushed"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) changes %d %s your branch also changes:\n",
		s.branch, where, len(s.files), plural(len(s.files), "file", "files"))
	writeFiles(&b, s.files)
	fmt.Fprintf(&b, "See all: git diff --name-only origin/%s...%s. Adapt your branch, or contact that branch's session if you both rework the same code.\n", def, s.branch)
	return b.String()
}

func writeFiles(b *strings.Builder, files paths) {
	for _, f := range files[:min(len(files), maxFiles)] {
		b.WriteString("- " + f + "\n")
	}
	if more := len(files) - maxFiles; more > 0 {
		fmt.Fprintf(b, "- and %d more\n", more)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
