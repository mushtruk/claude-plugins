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
	Upstream []string         `json:"upstream,omitempty"` // overlapping upstream commit shas
	Siblings map[string]paths `json:"siblings,omitempty"` // branch to overlapping files
}

func fingerprintOf(r report) fingerprint {
	f := fingerprint{Upstream: r.upstream.shas}
	for _, s := range r.siblings {
		if f.Siblings == nil {
			f.Siblings = make(map[string]paths)
		}
		f.Siblings[s.branch] = s.files
	}
	return f
}

// news keeps the sections of r the session hasn't heard: upstream when it
// has an overlapping commit not seen before, since the default branch is
// never rewritten; a sibling when it has a file not seen before, since
// siblings rebase and their shas change.
func (f fingerprint) news(r report) report {
	out := report{pushed: r.pushed, stale: r.stale}
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
