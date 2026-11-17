package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxText, maxPaths and maxPathBytes keep one entry under about 900
	// rendered characters, so several fit the notice cap.
	maxText      = 300
	maxPaths     = 10
	maxPathBytes = 200
	// maxLine bounds a board line, newline included, to one page: a single
	// O_APPEND write that size lands whole on a local filesystem, so
	// worktrees appending at once never interleave and the board needs no
	// lock.
	maxLine = 4096
	// boardTail bounds every read: over a thousand entries, months of
	// posting.
	boardTail = 1 << 20
	// announcementTTL covers the life of a branch.
	announcementTTL = 14 * 24 * time.Hour
)

// lineTooLongError is an entry whose board line would exceed maxLine.
type lineTooLongError struct {
	size int
}

func (e *lineTooLongError) Error() string {
	return fmt.Sprintf("board line is %d bytes, over the %d-byte limit", e.size, maxLine)
}

// posted is what every board entry carries: who posted it, from where, when.
type posted struct {
	Time   time.Time `json:"time"`
	Branch string    `json:"branch"`
	Head   string    `json:"head"`
	id     string    // first 16 hex characters of the line's sha256
	seq    int       // the line's position in the read, ordering every kind
}

// patterns are the repo-relative paths an entry is about: a plain path
// covers that file or directory, one with *?[ is a path.Match pattern.
type patterns []string

func (ps patterns) validate() error {
	if len(ps) > maxPaths {
		return fmt.Errorf("%d paths, at most %d allowed", len(ps), maxPaths)
	}
	for _, p := range ps {
		if len(p) > maxPathBytes {
			return fmt.Errorf("path %q is over %d bytes", p, maxPathBytes)
		}
		if path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return fmt.Errorf("path %q is not a clean repo-relative path", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("path %q is not a valid pattern", p)
		}
	}
	return nil
}

// matches reports whether any file falls under ps; no patterns cover every
// file.
func (ps patterns) matches(files paths) bool {
	if len(ps) == 0 {
		return true
	}
	for _, f := range files {
		for _, p := range ps {
			if covers(p, f) {
				return true
			}
		}
	}
	return false
}

func covers(pattern, file string) bool {
	if strings.ContainsAny(pattern, "*?[") {
		ok, _ := path.Match(pattern, file)
		return ok
	}
	return file == pattern || strings.HasPrefix(file, pattern+"/")
}

func validText(name, s string) error {
	if s == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if utf8.RuneCountInString(s) > maxText {
		return fmt.Errorf("%s is over %d characters", name, maxText)
	}
	return nil
}

type announcementPayload struct {
	Paths   patterns `json:"paths,omitempty"`
	Summary string   `json:"summary"`
	Action  string   `json:"action"`
}

func (p announcementPayload) validate() error {
	return errors.Join(validText("summary", p.Summary), validText("action", p.Action), p.Paths.validate())
}

type claimPayload struct {
	Paths   patterns `json:"paths"`
	Summary string   `json:"summary"`
}

func (p claimPayload) validate() error {
	if len(p.Paths) == 0 {
		return errors.New("a claim needs at least one path")
	}
	return errors.Join(validText("summary", p.Summary), p.Paths.validate())
}

type releasePayload struct{}

// announcement tells other branches' sessions about a contract change.
type announcement struct {
	posted
	announcementPayload
}

// newAnnouncement validates an announcement for posting.
func newAnnouncement(p posted, paths []string, summary, action string) (announcement, error) {
	a := announcement{posted: p, announcementPayload: announcementPayload{
		Paths:   paths,
		Summary: strings.TrimSpace(summary),
		Action:  strings.TrimSpace(action),
	}}
	if err := a.validate(); err != nil {
		return announcement{}, err
	}
	if _, err := a.line().encode(); err != nil {
		return announcement{}, err
	}
	return a, nil
}

func (a announcement) line() line {
	return line{posted: a.posted, Announcement: &a.announcementPayload}
}

// claim marks surfaces a branch is reshaping.
type claim struct {
	posted
	claimPayload
}

// release ends every earlier claim of its branch.
type release struct {
	posted
}

// line is a board line as stored: the envelope and exactly one payload.
type line struct {
	posted
	Announcement *announcementPayload `json:"announcement,omitempty"`
	Claim        *claimPayload        `json:"claim,omitempty"`
	Release      *releasePayload      `json:"release,omitempty"`
}

// encode is l's board line without its newline, or a *lineTooLongError.
func (l line) encode() ([]byte, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	if size := len(raw) + 1; size > maxLine {
		return nil, &lineTooLongError{size: size}
	}
	return raw, nil
}

func lineID(raw []byte) string {
	return shortHash(string(raw))
}

// entries are a board's valid lines, one slice per kind, in line order.
type entries struct {
	announcements []announcement
	claims        []claim
	releases      []release
}

// add files l under its kind; false when l holds no payload, several, or
// an invalid one.
func (e *entries) add(l line) bool {
	n := 0
	for _, set := range []bool{l.Announcement != nil, l.Claim != nil, l.Release != nil} {
		if set {
			n++
		}
	}
	switch {
	case n != 1:
		return false
	case l.Announcement != nil:
		a := announcement{posted: l.posted, announcementPayload: *l.Announcement}
		if a.validate() != nil {
			return false
		}
		e.announcements = append(e.announcements, a)
	case l.Claim != nil:
		c := claim{posted: l.posted, claimPayload: *l.Claim}
		if c.validate() != nil {
			return false
		}
		e.claims = append(e.claims, c)
	default:
		e.releases = append(e.releases, release{posted: l.posted})
	}
	return true
}

// recentAnnouncements returns the announcements posted within
// announcementTTL before now.
func (e entries) recentAnnouncements(now time.Time) []announcement {
	var out []announcement
	for _, a := range e.announcements {
		if now.Sub(a.Time) <= announcementTTL {
			out = append(out, a)
		}
	}
	return out
}

// concerns reports whether a is news for a branch changing files: it comes
// from another branch and touches one of them.
func (a announcement) concerns(branch string, files paths) bool {
	return a.Branch != branch && a.Paths.matches(files)
}

// board is a repo's append-only log of announcements, claims and releases,
// shared by all its worktrees.
type board struct {
	path string
}

// append writes l as one line and returns its id.
func (b board) append(l line) (string, error) {
	raw, err := l.encode()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(b.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(append(raw, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", err
	}
	return lineID(raw), nil
}

// read returns the valid entries in the board's last boardTail bytes and
// how many lines it skipped. A missing board is empty.
func (b board) read() (entries, int, error) {
	raw, err := b.tail()
	if err != nil {
		return entries{}, 0, err
	}
	var (
		e       entries
		skipped int
		seq     int
	)
	for l := range bytes.SplitSeq(raw, []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var parsed line
		if json.Unmarshal(l, &parsed) != nil {
			skipped++
			continue
		}
		parsed.id, parsed.seq = lineID(l), seq
		seq++
		if !e.add(parsed) {
			skipped++
		}
	}
	return e, skipped, nil
}

// tail returns the whole lines in the board's last boardTail bytes.
func (b board) tail() ([]byte, error) {
	f, err := os.Open(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() <= boardTail {
		return io.ReadAll(f)
	}
	// One byte early, so a cut that falls exactly on a line start keeps
	// that line.
	raw, err := io.ReadAll(io.NewSectionReader(f, fi.Size()-boardTail-1, boardTail+1))
	if err != nil {
		return nil, err
	}
	_, rest, _ := bytes.Cut(raw, []byte("\n"))
	return rest, nil
}
