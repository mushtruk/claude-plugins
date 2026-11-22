package main

import (
	"slices"
	"strings"
)

// claimFinding is another branch's live claim over files this branch
// changes.
type claimFinding struct {
	claim
	files paths
}

// claimants returns the branches that posted claims, sorted.
func (e entries) claimants() []string {
	var bs []string
	for _, c := range e.claims {
		bs = append(bs, c.Branch)
	}
	slices.Sort(bs)
	return slices.Compact(bs)
}

// held returns the claims no later release of their branch ended. Board
// order decides "later", so a claim and a release in the same second still
// order.
func (e entries) held() []claim {
	last := make(map[string]int, len(e.releases))
	for _, r := range e.releases {
		last[r.Branch] = max(last[r.Branch], r.seq+1)
	}
	return slices.DeleteFunc(slices.Clone(e.claims), func(c claim) bool {
		return c.seq < last[c.Branch]
	})
}

// live returns the held claims whose branch has not landed, given the tips
// landing reported for the claimants. A branch landed when its ref is gone,
// its tip merged, or its ref was created after the claim, which means the
// name was reused.
func (e entries) live(tips map[string]tip) []claim {
	return slices.DeleteFunc(e.held(), func(c claim) bool {
		t, ok := tips[c.Branch]
		return !ok || t.merged || t.created.After(c.Time)
	})
}

// claimsOver returns the claims of branches other than branch that files
// fall inside, each with the files it covers.
func claimsOver(claims []claim, branch string, files paths) []claimFinding {
	var out []claimFinding
	for _, c := range claims {
		if c.Branch == branch {
			continue
		}
		if inside := c.Paths.covered(files); len(inside) > 0 {
			out = append(out, claimFinding{claim: c, files: inside})
		}
	}
	return out
}

// overlaps reports whether some path of ps and some path of qs could cover
// the same file. A pattern stands for the directory before its first
// wildcard, so globs over-warn rather than miss.
func (ps patterns) overlaps(qs patterns) bool {
	for _, p := range ps {
		for _, q := range qs {
			a, b := literalDir(p), literalDir(q)
			if within(a, b) || within(b, a) {
				return true
			}
		}
	}
	return false
}

// literalDir is p itself when it has no wildcard, else the directory that
// holds everything before its first wildcard ("" for the repo root).
func literalDir(p string) string {
	i := strings.IndexAny(p, "*?[")
	if i < 0 {
		return p
	}
	return p[:max(strings.LastIndex(p[:i], "/"), 0)]
}
