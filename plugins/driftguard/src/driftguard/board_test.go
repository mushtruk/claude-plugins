package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testPosted = posted{
	Time:   time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	Branch: "feat/x",
	Head:   strings.Repeat("a", 40),
}

func longPaths(n, size int) []string {
	ps := make([]string, n)
	for i := range ps {
		ps[i] = fmt.Sprintf("p%d/", i) + strings.Repeat("x", size-3)
	}
	return ps
}

func TestNewAnnouncementValidates(t *testing.T) {
	t.Parallel()
	wide := strings.Repeat("\U0001D11E", 300) // 300 four-byte runes
	tests := []struct {
		name            string
		paths           []string
		summary, action string
		wantErr         bool
	}{
		{name: "valid", paths: []string{"pkg/m", "*.proto"}, summary: "renamed Send", action: "call Deliver"},
		{name: "valid without paths", summary: "s", action: "a"},
		{name: "empty summary", summary: "  ", action: "a", wantErr: true},
		{name: "empty action", summary: "s", action: "", wantErr: true},
		{name: "301-rune summary", summary: strings.Repeat("s", 301), action: "a", wantErr: true},
		{name: "11 paths", paths: longPaths(11, 5), summary: "s", action: "a", wantErr: true},
		{name: "201-byte path", paths: longPaths(1, 201), summary: "s", action: "a", wantErr: true},
		{name: "absolute path", paths: []string{"/etc"}, summary: "s", action: "a", wantErr: true},
		{name: "dot-dot path", paths: []string{"a/../b"}, summary: "s", action: "a", wantErr: true},
		{name: "leading dot-dot", paths: []string{"../b"}, summary: "s", action: "a", wantErr: true},
		{name: "bad pattern", paths: []string{"["}, summary: "s", action: "a", wantErr: true},
		{name: "line too long", paths: longPaths(10, 200), summary: wide, action: wide, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newAnnouncement(testPosted, tt.paths, tt.summary, tt.action)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewAnnouncementLineTooLong(t *testing.T) {
	t.Parallel()
	wide := strings.Repeat("\U0001D11E", 300)
	_, err := newAnnouncement(testPosted, longPaths(10, 200), wide, wide)
	var tooLong *lineTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v, want *lineTooLongError", err)
	}
	if tooLong.size <= maxLine {
		t.Errorf("size = %d, want over %d", tooLong.size, maxLine)
	}
}

// maximalAnnouncement is a valid announcement whose line, newline included,
// is exactly maxLine bytes.
func maximalAnnouncement(t *testing.T, i int) announcement {
	t.Helper()
	summary := fmt.Sprintf("writer %d ", i) + strings.Repeat("\U0001D11E", 280)
	action := ""
	for _, unit := range []string{"\U0001D11E", "x"} {
		for {
			a, err := newAnnouncement(testPosted, longPaths(10, 200), summary, action+unit)
			var tooLong *lineTooLongError
			if errors.As(err, &tooLong) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			action += unit
			if raw, _ := a.line().encode(); len(raw)+1 == maxLine {
				return a
			}
		}
	}
	t.Fatal("no announcement fills a line exactly")
	return announcement{}
}

func TestBoardParallelAppendsStayWhole(t *testing.T) {
	t.Parallel()
	b := board{path: filepath.Join(t.TempDir(), "driftguard", "board.jsonl")}
	const writers = 32
	lines := make([]line, writers)
	for i := range lines {
		lines[i] = maximalAnnouncement(t, i).line()
	}
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range lines {
		wg.Go(func() { _, errs[i] = b.append(lines[i]) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	e, skipped, err := b.read()
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(e.announcements) != writers {
		t.Fatalf("read %d announcements, skipped %d; want %d, 0", len(e.announcements), skipped, writers)
	}
}

func TestPatternsMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		patterns patterns
		files    paths
		want     bool
	}{
		{name: "directory prefix", patterns: patterns{"gopkg/mailer"}, files: paths{"gopkg/mailer/send.go"}, want: true},
		{name: "prefix not on a boundary", patterns: patterns{"gopkg/mailer"}, files: paths{"gopkg/mailerx/a.go"}},
		{name: "exact file", patterns: patterns{"a.go"}, files: paths{"a.go"}, want: true},
		{name: "glob", patterns: patterns{"proto/*.proto"}, files: paths{"proto/user.proto"}, want: true},
		{name: "star does not cross a slash", patterns: patterns{"proto/*.proto"}, files: paths{"proto/v1/user.proto"}},
		{name: "no match", patterns: patterns{"b.go"}, files: paths{"a.go"}},
		{name: "no paths matches all", patterns: nil, files: paths{"a.go"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.patterns.matches(tt.files); got != tt.want {
				t.Errorf("matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func writeBoard(t *testing.T, body string) board {
	t.Helper()
	path := filepath.Join(t.TempDir(), "board.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return board{path: path}
}

func TestBoardReadSortsKindsAndSkipsBadLines(t *testing.T) {
	t.Parallel()
	env := `"time":"2026-03-01T09:00:00Z","branch":"feat/x","head":"` + strings.Repeat("a", 40) + `"`
	b := writeBoard(t, strings.Join([]string{
		`{` + env + `,"announcement":{"paths":["pkg/m"],"summary":"s","action":"a"}}`,
		`{` + env + `,"claim":{"paths":["pkg/m"],"summary":"s"}}`,
		`{` + env + `,"release":{}}`,
		`not json`,
		`{` + env + `}`,
		`{` + env + `,"release":{},"claim":{"paths":["a.go"],"summary":"s"}}`,
		`{` + env + `,"announcement":{"summary":"","action":"a"}}`,
		`{` + env + `,"claim":{"summary":"no paths"}}`,
		``,
	}, "\n"))
	e, skipped, err := b.read()
	if err != nil {
		t.Fatal(err)
	}
	if len(e.announcements) != 1 || len(e.claims) != 1 || len(e.releases) != 1 {
		t.Fatalf("read %d announcements, %d claims, %d releases; want 1 each", len(e.announcements), len(e.claims), len(e.releases))
	}
	if skipped != 5 {
		t.Errorf("skipped = %d, want 5", skipped)
	}
	a := e.announcements[0]
	if a.Branch != "feat/x" || a.Summary != "s" || a.Action != "a" || len(a.Paths) != 1 || a.id == "" {
		t.Errorf("announcement = %+v", a)
	}
	if e.announcements[0].seq >= e.claims[0].seq || e.claims[0].seq >= e.releases[0].seq {
		t.Error("seq does not follow line order")
	}
}

func TestBoardReadsOnlyTheTail(t *testing.T) {
	t.Parallel()
	b := board{path: filepath.Join(t.TempDir(), "board.jsonl")}
	l := maximalAnnouncement(t, 0).line()
	n := 0
	for size := int64(0); size <= boardTail+maxLine; n++ {
		if _, err := b.append(l); err != nil {
			t.Fatal(err)
		}
		size += maxLine
	}
	e, skipped, err := b.read()
	if err != nil {
		t.Fatal(err)
	}
	if want := boardTail / maxLine; len(e.announcements) != want || skipped != 0 {
		t.Fatalf("read %d of %d announcements, skipped %d; want %d, 0", len(e.announcements), n, skipped, want)
	}
}

func TestBoardReadMissingIsEmpty(t *testing.T) {
	t.Parallel()
	e, skipped, err := board{path: filepath.Join(t.TempDir(), "none.jsonl")}.read()
	if err != nil || skipped != 0 || len(e.announcements)+len(e.claims)+len(e.releases) != 0 {
		t.Fatalf("read = %+v, %d, %v; want empty", e, skipped, err)
	}
}
