package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// fingerprint is the overlap a worktree's session was last told about.
type fingerprint struct {
	Claims   map[string]paths `json:"claims,omitempty"`   // claim id to the files inside it
	Upstream []string         `json:"upstream,omitempty"` // overlapping upstream commit shas
	Siblings map[string]paths `json:"siblings,omitempty"` // branch to overlapping files
}

func fingerprintOf(r report) fingerprint {
	f := fingerprint{Upstream: r.upstream.shas}
	for _, c := range r.claims {
		if f.Claims == nil {
			f.Claims = make(map[string]paths)
		}
		f.Claims[c.id] = c.files
	}
	for _, s := range r.siblings {
		if f.Siblings == nil {
			f.Siblings = make(map[string]paths)
		}
		f.Siblings[s.branch] = s.files
	}
	return f
}

// news keeps the sections of r the session hasn't heard: a claim when one
// of this branch's files inside it was not warned before; upstream when it
// has an overlapping commit not seen before, since the default branch is
// never rewritten; a sibling when it has a file not seen before, since
// siblings rebase and their shas change. Announcements pass through: what a
// session saw of them is tracked apart, in state.Seen.
func (f fingerprint) news(r report) report {
	out := report{announcements: r.announcements, pushed: r.pushed, stale: r.stale}
	for _, c := range r.claims {
		if len(c.files.intersect(f.Claims[c.id])) < len(c.files) {
			out.claims = append(out.claims, c)
		}
	}
	for _, sha := range r.upstream.shas {
		if !slices.Contains(f.Upstream, sha) {
			out.upstream = r.upstream
			break
		}
	}
	for _, s := range r.siblings {
		if len(s.files.intersect(f.Siblings[s.branch])) < len(s.files) {
			out.siblings = append(out.siblings, s)
		}
	}
	return out
}

// state is what a worktree keeps between runs.
type state struct {
	LastCheck   time.Time   `json:"lastCheck"`
	Fingerprint fingerprint `json:"fingerprint"`
	Seen        []string    `json:"seen,omitempty"` // ids of announcements shown here
}

// unseen drops the announcements already shown in this worktree.
func (s state) unseen(as []announcement) []announcement {
	seen := make(map[string]bool, len(s.Seen))
	for _, id := range s.Seen {
		seen[id] = true
	}
	return slices.DeleteFunc(slices.Clone(as), func(a announcement) bool { return seen[a.id] })
}

// see records shown as seen and forgets ids no longer in current, the
// announcements still on the board and inside the window.
func (s *state) see(shown, current []announcement) {
	seen := make(map[string]bool, len(s.Seen)+len(shown))
	for _, id := range s.Seen {
		seen[id] = true
	}
	for _, a := range shown {
		seen[a.id] = true
	}
	s.Seen = nil
	for _, a := range current {
		if seen[a.id] {
			s.Seen = append(s.Seen, a.id)
			delete(seen, a.id)
		}
	}
}

// fetchState is when any worktree of a repo last fetched; they share origin's
// refs, so one fetch per window serves them all.
type fetchState struct {
	LastFetch time.Time `json:"lastFetch"`
}

// states stores per-worktree state and per-repo fetch times as JSON files.
type states struct {
	dir string
}

// load returns the state saved for root. A missing file is the zero state;
// a corrupt one is the zero state and an error for the caller to log.
func (s states) load(root string) (state, error) {
	var st state
	if err := s.read(s.worktreeFile(root), &st); err != nil {
		return state{}, err
	}
	return st, nil
}

func (s states) save(root string, st state) error {
	return s.write(s.worktreeFile(root), st)
}

// lastFetch returns when the repo with this common dir last fetched, zero
// when unknown.
func (s states) lastFetch(commonDir string) time.Time {
	var fs fetchState
	if s.read(s.fetchFile(commonDir), &fs) != nil {
		return time.Time{}
	}
	return fs.LastFetch
}

func (s states) saveFetch(commonDir string, t time.Time) error {
	return s.write(s.fetchFile(commonDir), fetchState{LastFetch: t})
}

func (s states) worktreeFile(root string) string {
	return filepath.Join(s.dir, filepath.Base(root)+"-"+shortHash(root)+".json")
}

func (s states) fetchFile(commonDir string) string {
	return filepath.Join(s.dir, "fetch-"+shortHash(commonDir)+".json")
}

func (s states) read(path string, v any) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// write replaces path through a rename, so a reader never sees half a file.
func (s states) write(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	if err := replace(tmp, raw, path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// replace writes raw to tmp, closes it and renames it over path.
func replace(tmp *os.File, raw []byte, path string) error {
	_, werr := tmp.Write(raw)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// shortHash is the first 16 hex characters of s's sha256.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
