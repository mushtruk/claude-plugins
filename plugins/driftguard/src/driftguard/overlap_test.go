package main

import (
	"reflect"
	"testing"
)

func TestPathsIntersect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b paths
		want paths
	}{
		{name: "both empty"},
		{name: "one empty", a: newPaths([]string{"a.go"})},
		{name: "disjoint", a: newPaths([]string{"a.go"}), b: newPaths([]string{"b.go"})},
		{
			name: "common files, unsorted and duplicated input",
			a:    newPaths([]string{"c.go", "a.go", "a.go", "pkg/m/m.go"}),
			b:    newPaths([]string{"pkg/m/m.go", "b.go", "a.go"}),
			want: paths{"a.go", "pkg/m/m.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.a.intersect(tt.b); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("intersect = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPathsUnion(t *testing.T) {
	t.Parallel()
	a := newPaths([]string{"b.go", "a.go"})
	b := newPaths([]string{"c.go", "a.go"})
	if got, want := a.union(b), (paths{"a.go", "b.go", "c.go"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("union = %v, want %v", got, want)
	}
}

func TestUpstreamOverlap(t *testing.T) {
	t.Parallel()
	commits := []commit{
		{sha: "s1", files: paths{"a.go", "c.go"}},
		{sha: "s2", files: paths{"b.go"}},
		{sha: "s3", files: paths{"a.go", "pkg/m/m.go"}},
		{sha: "s4"},
	}
	tests := []struct {
		name    string
		commits []commit
		files   paths
		want    upstreamFinding
	}{
		{name: "no upstream commits", files: paths{"a.go"}},
		{name: "no overlap", commits: commits, files: paths{"z.go"}, want: upstreamFinding{commits: 4}},
		{
			name:    "overlapping commits and the union of their shared files",
			commits: commits,
			files:   paths{"a.go", "pkg/m/m.go", "z.go"},
			want:    upstreamFinding{commits: 4, shas: []string{"s1", "s3"}, files: paths{"a.go", "pkg/m/m.go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := upstreamOverlap(tt.commits, tt.files); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("upstreamOverlap = %+v, want %+v", got, tt.want)
			}
		})
	}
}
