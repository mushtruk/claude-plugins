package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logOnce opens the log in dir, writes one line and closes it.
func logOnce(t *testing.T, dir string) {
	t.Helper()
	logger, closer, err := openLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("run", "decision", "test")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOpenLogCreatesMissingDirAndAppends(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state", "driftguard")
	logOnce(t, dir)
	logOnce(t, dir)
	got := readFile(t, filepath.Join(dir, "log.jsonl"))
	if n := strings.Count(got, "\n"); n != 2 || !strings.Contains(got, `"decision":"test"`) {
		t.Fatalf("log holds %d lines, want 2 JSON lines:\n%s", n, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "log.jsonl.1")); !os.IsNotExist(err) {
		t.Fatalf("a log under the cap was rotated: %v", err)
	}
}

func TestOpenLogRotatesOverCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := bytes.Repeat([]byte("x"), logMax+1)
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl.1"), []byte("older\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logOnce(t, dir)
	if got := readFile(t, filepath.Join(dir, "log.jsonl.1")); len(got) != logMax+1 {
		t.Fatalf("log.jsonl.1 has %d bytes, want the rotated %d", len(got), logMax+1)
	}
	if got := readFile(t, filepath.Join(dir, "log.jsonl")); strings.Count(got, "\n") != 1 || strings.Contains(got, "xxx") {
		t.Fatalf("log.jsonl is not a fresh file:\n%.200s", got)
	}
}
