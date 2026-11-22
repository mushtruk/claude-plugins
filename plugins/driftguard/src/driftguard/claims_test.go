package main

import (
	"reflect"
	"testing"
	"time"
)

var claimTime = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func testClaim(id, branch string, seq int, at time.Time, ps ...string) claim {
	return claim{
		posted:       posted{Time: at, Branch: branch, id: id, seq: seq},
		claimPayload: claimPayload{Paths: ps, Summary: "summary " + id},
	}
}

func testRelease(branch string, seq int, at time.Time) release {
	return release{posted: posted{Time: at, Branch: branch, seq: seq}}
}

func TestEntriesLive(t *testing.T) {
	t.Parallel()
	before, after := claimTime.Add(-time.Hour), claimTime.Add(time.Hour)
	open := map[string]tip{"feat/x": {sha: "x1", created: before}, "feat/y": {sha: "y1", created: before}}
	tests := []struct {
		name     string
		claims   []claim
		releases []release
		tips     map[string]tip
		want     []string
	}{
		{
			name:   "no release, unlanded",
			claims: []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			tips:   open,
			want:   []string{"c1"},
		},
		{
			name:     "later release from its branch",
			claims:   []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			releases: []release{testRelease("feat/x", 1, after)},
			tips:     open,
		},
		{
			name:     "release from another branch",
			claims:   []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			releases: []release{testRelease("feat/y", 1, after)},
			tips:     open,
			want:     []string{"c1"},
		},
		{
			name:     "claim posted after a release",
			claims:   []claim{testClaim("c1", "feat/x", 1, after, "pkg/m")},
			releases: []release{testRelease("feat/x", 0, claimTime)},
			tips:     open,
			want:     []string{"c1"},
		},
		{
			name:     "same second, release on a later line",
			claims:   []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			releases: []release{testRelease("feat/x", 1, claimTime)},
			tips:     open,
		},
		{
			name:     "same second, claim on a later line",
			claims:   []claim{testClaim("c1", "feat/x", 1, claimTime, "pkg/m")},
			releases: []release{testRelease("feat/x", 0, claimTime)},
			tips:     open,
			want:     []string{"c1"},
		},
		{
			name:   "tip merged",
			claims: []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			tips:   map[string]tip{"feat/x": {sha: "x1", merged: true, created: before}},
		},
		{
			name:   "branch absent",
			claims: []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			tips:   map[string]tip{},
		},
		{
			name:   "branch name reused after landing",
			claims: []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			tips:   map[string]tip{"feat/x": {sha: "x9", created: after}},
		},
		{
			name:   "rebased unpushed branch",
			claims: []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			tips:   map[string]tip{"feat/x": {sha: "x2", created: before}},
			want:   []string{"c1"},
		},
		{
			name:   "creation unknown",
			claims: []claim{testClaim("c1", "feat/x", 0, claimTime, "pkg/m")},
			tips:   map[string]tip{"feat/x": {sha: "x1"}},
			want:   []string{"c1"},
		},
		{
			name: "release ends only its own branch's earlier claims",
			claims: []claim{
				testClaim("c1", "feat/x", 0, claimTime, "pkg/m"),
				testClaim("c2", "feat/y", 1, claimTime, "pkg/n"),
				testClaim("c3", "feat/x", 3, after, "pkg/o"),
			},
			releases: []release{testRelease("feat/x", 2, claimTime)},
			tips:     open,
			want:     []string{"c2", "c3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := entries{claims: tt.claims, releases: tt.releases}
			var got []string
			for _, c := range e.live(tt.tips) {
				got = append(got, c.id)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("live = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEntriesClaimants(t *testing.T) {
	t.Parallel()
	e := entries{claims: []claim{
		testClaim("c1", "feat/y", 0, claimTime, "a"),
		testClaim("c2", "feat/x", 1, claimTime, "b"),
		testClaim("c3", "feat/y", 2, claimTime, "c"),
	}}
	if got, want := e.claimants(), []string{"feat/x", "feat/y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("claimants = %v, want %v", got, want)
	}
}

func TestPatternsOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want bool
	}{
		{a: "gopkg/mailer", b: "gopkg/mailer/send.go", want: true},
		{a: "gopkg/mailer", b: "gopkg/mailer", want: true},
		{a: "gopkg/mailer", b: "gopkg/mailerx", want: false},
		{a: "proto/*/mail.proto", b: "proto/fga", want: true},
		{a: "*.proto", b: "gopkg/mailer", want: true},
		{a: "gopkg/mailer", b: "services/delivery", want: false},
		{a: "proto/fga/*.proto", b: "proto/billing", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.a+" vs "+tt.b, func(t *testing.T) {
			t.Parallel()
			for _, pair := range [][2]patterns{{{tt.a}, {tt.b}}, {{tt.b}, {tt.a}}} {
				if got := pair[0].overlaps(pair[1]); got != tt.want {
					t.Errorf("%v overlaps %v = %v, want %v", pair[0], pair[1], got, tt.want)
				}
			}
		})
	}
}

func TestClaimsOver(t *testing.T) {
	t.Parallel()
	claims := []claim{
		testClaim("own", "feat/me", 0, claimTime, "pkg/m"),
		testClaim("c1", "feat/x", 1, claimTime, "pkg/m", "*.proto"),
		testClaim("c2", "feat/y", 2, claimTime, "services"),
	}
	files := newPaths([]string{"a.proto", "b.go", "pkg/m/m.go", "pkg/mx/x.go"})
	got := claimsOver(claims, "feat/me", files)
	if len(got) != 1 || got[0].id != "c1" {
		t.Fatalf("claimsOver = %+v, want only c1", got)
	}
	if want := (paths{"a.proto", "pkg/m/m.go"}); !reflect.DeepEqual(got[0].files, want) {
		t.Errorf("files = %v, want %v", got[0].files, want)
	}
}
