package main

import (
	"slices"
)

// paths is a sorted, duplicate-free list of repo-relative file paths.
type paths []string

func newPaths(ps []string) paths {
	if len(ps) == 0 {
		return nil
	}
	out := slices.Clone(ps)
	slices.Sort(out)
	return slices.Compact(out)
}

// intersect returns the paths in both p and q.
func (p paths) intersect(q paths) paths {
	var out paths
	for i, j := 0, 0; i < len(p) && j < len(q); {
		switch {
		case p[i] < q[j]:
			i++
		case p[i] > q[j]:
			j++
		default:
			out = append(out, p[i])
			i++
			j++
		}
	}
	return out
}

// union returns the paths in p or q.
func (p paths) union(q paths) paths {
	return newPaths(append(slices.Clone(p), q...))
}

// commit is an upstream commit and the files it changes.
type commit struct {
	sha   string
	files paths
}

// upstreamFinding is how far the default branch moved past this branch's
// merge base, and which of those commits change this branch's files.
type upstreamFinding struct {
	commits int
	shas    []string // the overlapping commits
	files   paths    // this branch's files they change
}

func upstreamOverlap(commits []commit, files paths) upstreamFinding {
	f := upstreamFinding{commits: len(commits)}
	var shared []string
	for _, c := range commits {
		ov := c.files.intersect(files)
		if len(ov) == 0 {
			continue
		}
		f.shas = append(f.shas, c.sha)
		shared = append(shared, ov...)
	}
	f.files = newPaths(shared)
	return f
}

// siblingFinding is a sibling branch's committed files that this branch also
// changes.
type siblingFinding struct {
	branch string
	pushed bool
	files  paths
}

// report is everything a check found for one branch.
type report struct {
	upstream upstreamFinding
	siblings []siblingFinding
	pushed   bool // this branch is on origin
	stale    bool // the fetch failed, so origin refs may be old
}
