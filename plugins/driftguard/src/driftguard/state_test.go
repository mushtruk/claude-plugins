package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatesRoundTrip(t *testing.T) {
	t.Parallel()
	s := states{dir: t.TempDir()}
	want := state{
		LastCheck: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Fingerprint: fingerprint{
			Upstream: []string{"s1", "s2"},
			Siblings: map[string]paths{"feat/x": {"a.go"}},
		},
	}
	if err := s.save("/repo/wt", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.load("/repo/wt")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("load = %+v, want %+v", got, want)
	}
	if other, err := s.load("/repo/other"); err != nil || !reflect.DeepEqual(other, state{}) {
		t.Fatalf("another root loads %+v, %v; want the zero state", other, err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "wt-") || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("state dir holds %v, want one wt-<hash>.json and no temp file", entries)
	}
}

func TestStatesLoadMissingAndCorrupt(t *testing.T) {
	t.Parallel()
	s := states{dir: t.TempDir()}
	if got, err := s.load("/repo/wt"); err != nil || !reflect.DeepEqual(got, state{}) {
		t.Fatalf("missing file loads %+v, %v; want the zero state", got, err)
	}
	if err := s.save("/repo/wt", state{LastCheck: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, entries[0].Name()), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.load("/repo/wt")
	if err == nil {
		t.Error("corrupt file loaded without an error to log")
	}
	if !reflect.DeepEqual(got, state{}) {
		t.Fatalf("corrupt file loads %+v, want the zero state", got)
	}
}

func TestStatesFetchWindowIsPerCommonDir(t *testing.T) {
	t.Parallel()
	s := states{dir: t.TempDir()}
	if got := s.lastFetch("/repo/a/.git"); !got.IsZero() {
		t.Fatalf("lastFetch before any save = %v, want zero", got)
	}
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.saveFetch("/repo/a/.git", when); err != nil {
		t.Fatal(err)
	}
	if got := s.lastFetch("/repo/a/.git"); !got.Equal(when) {
		t.Fatalf("lastFetch = %v, want %v", got, when)
	}
	if got := s.lastFetch("/repo/b/.git"); !got.IsZero() {
		t.Fatalf("another repo's lastFetch = %v, want zero", got)
	}
}

func TestFingerprintNews(t *testing.T) {
	t.Parallel()
	current := report{
		claims: []claimFinding{
			{claim: testClaim("c1", "feat/x", 0, claimTime, "pkg"), files: paths{"pkg/a.go", "pkg/b.go"}},
			{claim: testClaim("c2", "feat/y", 1, claimTime, "lib"), files: paths{"lib/c.go"}},
		},
		upstream: upstreamFinding{commits: 3, shas: []string{"s1", "s2"}, files: paths{"a.go"}},
		siblings: []siblingFinding{
			{branch: "feat/x", files: paths{"a.go", "b.go"}},
			{branch: "feat/y", files: paths{"c.go"}},
		},
		stale: true,
	}
	tests := []struct {
		name string
		prev fingerprint
		want report
	}{
		{name: "empty previous makes everything news", want: current},
		{
			name: "known shas and file subsets are no news",
			prev: fingerprint{
				Claims:   map[string]paths{"c1": {"pkg/a.go", "pkg/b.go"}, "c2": {"lib/c.go", "lib/d.go"}},
				Upstream: []string{"s1", "s2", "s0"},
				Siblings: map[string]paths{"feat/x": {"a.go", "b.go", "z.go"}, "feat/y": {"c.go"}},
			},
			want: report{stale: true},
		},
		{
			name: "a new file inside a known claim, a new upstream sha and a new sibling file are news",
			prev: fingerprint{
				Claims:   map[string]paths{"c1": {"pkg/a.go"}, "c2": {"lib/c.go"}},
				Upstream: []string{"s1"},
				Siblings: map[string]paths{"feat/x": {"a.go"}, "feat/y": {"c.go"}},
			},
			want: report{
				claims:   current.claims[:1],
				upstream: current.upstream,
				siblings: current.siblings[:1],
				stale:    true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.prev.news(current); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("news =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestFingerprintOfReportHasNoNews(t *testing.T) {
	t.Parallel()
	r := report{
		claims:   []claimFinding{{claim: testClaim("c1", "feat/x", 0, claimTime, "pkg"), files: paths{"pkg/a.go"}}},
		upstream: upstreamFinding{commits: 3, shas: []string{"s1"}, files: paths{"a.go"}},
		siblings: []siblingFinding{{branch: "feat/x", pushed: true, files: paths{"a.go"}}},
		pushed:   true,
	}
	if got, want := fingerprintOf(r).news(r), (report{pushed: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("news = %+v, want %+v", got, want)
	}
}
