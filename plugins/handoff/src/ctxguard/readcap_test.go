package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// hundredByteLines returns n lines that are 100 bytes each once writeLines
// adds the newline.
func hundredByteLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat("x", 99)
	}
	return out
}

func readEvent(agent, cwd string, input map[string]any) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "session_id": "sess", "agent_id": agent, "cwd": cwd,
		"tool_name": "Read", "tool_input": input}
}

func bashEvent(cwd, command string) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "session_id": "sess", "agent_id": "a1", "cwd": cwd,
		"tool_name": "Bash", "tool_input": map[string]any{"command": command}}
}

// loggedDecision runs the hook with a fresh log and returns its output and
// the decision it logged.
func loggedDecision(t *testing.T, in map[string]any) (map[string]any, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("CTXGUARD_LOG", log)
	out := runHook(t, in)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(b), "\n")
	_, decision, _ := strings.Cut(head, " decision=")
	return out, decision
}

func TestReadCap(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	small := writeLines(t, filepath.Join(dir, "small.go"), hundredByteLines(10)...)
	big := writeLines(t, filepath.Join(dir, "big.go"), hundredByteLines(1000)...)
	huge := writeLines(t, filepath.Join(dir, "huge.go"), hundredByteLines(3000)...)
	writeLines(t, filepath.Join(dir, "mid1.go"), hundredByteLines(50)...)
	writeLines(t, filepath.Join(dir, "mid2.go"), hundredByteLines(50)...)
	bin := writeLines(t, filepath.Join(dir, "blob.dat"), append([]string{"\x00"}, hundredByteLines(1000)...)...)
	img := writeLines(t, filepath.Join(dir, "shot.png"), hundredByteLines(1000)...)
	// Line 11 is longer than the 64 KiB line buffer.
	long := writeLines(t, filepath.Join(dir, "min.js"), append(hundredByteLines(10), strings.Repeat("x", 100000))...)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	// Read adds a 7-byte prefix, so a 100-byte line counts 107; the scan
	// stops at the first line past the 8192-byte cap.
	cases := []struct {
		name, env string
		ev        map[string]any
		decision  string
	}{
		{"read small whole", "", readEvent("a1", dir, map[string]any{"file_path": small}), "read-ok 1070"},
		{"read big whole", "", readEvent("a1", dir, map[string]any{"file_path": big}), "read-capped 8239"},
		{"read big small range", "", readEvent("a1", dir, map[string]any{"file_path": big, "offset": 10, "limit": 40}), "read-ok 4280"},
		{"read big range over cap", "", readEvent("a1", dir, map[string]any{"file_path": big, "offset": 10, "limit": 500}), "read-capped 8239"},
		{"read offset is 1-based", "", readEvent("a1", dir, map[string]any{"file_path": big, "offset": 950}), "read-ok 5457"},
		{"read range clipped at the end", "", readEvent("a1", dir, map[string]any{"file_path": big, "offset": 999, "limit": 5}), "read-ok 214"},
		{"read offset 0 is line 1", "", readEvent("a1", dir, map[string]any{"file_path": big, "offset": 0, "limit": 3}), "read-ok 321"},
		{"read offset only over cap", "", readEvent("a1", dir, map[string]any{"file_path": big, "offset": 500}), "read-capped 8239"},
		{"read limit only", "", readEvent("a1", dir, map[string]any{"file_path": big, "limit": 50}), "read-ok 5350"},
		{"read default limit 2000 lines", "250000", readEvent("a1", dir, map[string]any{"file_path": huge}), "read-ok 214000"},
		{"read limit 0 is the default", "250000", readEvent("a1", dir, map[string]any{"file_path": huge, "limit": 0}), "read-ok 214000"},
		{"read binary", "", readEvent("a1", dir, map[string]any{"file_path": bin}), "read-ok 0"},
		{"read image extension", "", readEvent("a1", dir, map[string]any{"file_path": img}), "read-ok 0"},
		{"read with pages", "", readEvent("a1", dir, map[string]any{"file_path": big, "pages": "1-3"}), "read-ok 0"},
		{"read line over the line buffer", "", readEvent("a1", dir, map[string]any{"file_path": long}), "read-capped 66613"},
		{"read fifo", "", readEvent("a1", dir, map[string]any{"file_path": fifo}), "read-error"},
		{"read missing file", "", readEvent("a1", dir, map[string]any{"file_path": filepath.Join(dir, "nope")}), "read-error"},
		{"main thread big read", "", readEvent("", dir, map[string]any{"file_path": big}), "read-main"},
		{"cap 0 turns it off", "0", readEvent("a1", dir, map[string]any{"file_path": big}), "read-off"},
		{"cap from env", "200000", readEvent("a1", dir, map[string]any{"file_path": big}), "read-ok 107000"},
		{"cat one file under cap", "", bashEvent(dir, "cat mid1.go"), "read-ok 5000"},
		{"cat two files summing over cap", "", bashEvent(dir, "cat mid1.go; cat mid2.go"), "read-capped 8200"},
		{"cat both in one call over cap", "", bashEvent(dir, "cat mid1.go mid2.go"), "read-capped 8200"},
		{"cd and sed over cap", "", bashEvent("/", "cd "+dir+" && sed -n '1,500p' big.go"), "read-capped 8200"},
		{"cd and sed under cap", "", bashEvent("/", "cd "+dir+" && sed -n '1,40p' big.go"), "read-ok 4000"},
		{"sed single line", "", bashEvent(dir, "sed -n '5p' big.go"), "read-ok 100"},
		{"head default", "", bashEvent(dir, "head big.go"), "read-ok 1000"},
		{"head -n over cap", "", bashEvent(dir, "head -n 2000 big.go"), "read-capped 8200"},
		{"head -N over cap", "", bashEvent(dir, "head -2000 big.go"), "read-capped 8200"},
		{"tail -n under cap", "", bashEvent(dir, "tail -n 20 big.go"), "read-ok 2000"},
		{"tail longer than the file", "", bashEvent(dir, "tail -n 50 small.go"), "read-ok 1000"},
		{"tail -n over cap", "", bashEvent(dir, "tail -n 2000 big.go"), "read-capped 8193"},
		{"tail -n +N unrecognised", "", bashEvent(dir, "tail -n +5 big.go"), "read-unrecognised"},
		{"nl over cap", "", bashEvent(dir, "nl big.go"), "read-capped 8239"},
		{"cat -n unrecognised", "", bashEvent(dir, "cat -n big.go"), "read-unrecognised"},
		{"pipe unrecognised", "", bashEvent(dir, "cat big.go | grep x"), "read-unrecognised"},
		{"git unrecognised", "", bashEvent(dir, "git diff"), "read-unrecognised"},
		{"bash missing file", "", bashEvent(dir, "cat nope.go"), "read-error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CTXGUARD_AGENT_READ_CAP", tc.env)
			out, decision := loggedDecision(t, tc.ev)
			if decision != tc.decision {
				t.Fatalf("decision %q, want %q", decision, tc.decision)
			}
			h, _ := out["hookSpecificOutput"].(map[string]any)
			denied := h["permissionDecision"] == "deny"
			if wantDeny := strings.HasPrefix(tc.decision, "read-capped"); denied != wantDeny || (!wantDeny && out != nil) {
				t.Fatalf("output %v, want deny=%v", out, wantDeny)
			}
		})
	}
}

func TestReadCapDenyJSON(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	big := writeLines(t, filepath.Join(dir, "big.go"), hundredByteLines(1000)...)
	long := writeLines(t, filepath.Join(dir, "min.js"), append(hundredByteLines(10), strings.Repeat("x", 100000))...)
	const head = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":` +
		`"READ CAP: this call would return more than the 8 KB a subagent may read at once (an estimate). `
	const tail = `Do not re-read content your brief already inlines."}}` + "\n"
	const longLine = "alone is longer than that: find what you need with `grep -n`, take only that part of the line with `cut -c`, " +
		"and read the lines around it in ranges that leave it out. "
	cases := []struct {
		name string
		ev   map[string]any
		want string
	}{
		// The scan stops after 77 lines (8239 bytes), so the budget is
		// 8192*77/8239 = 76 lines.
		{"line budget", readEvent("a1", "/", map[string]any{"file_path": big}),
			head + "Find the place with `grep -n`, then read at most 76 lines of it with Read offset/limit or `sed -n 'A,Bp'`. " + tail},
		{"long line inside the range", readEvent("a1", "/", map[string]any{"file_path": long}),
			head + "Line 11 " + longLine + tail},
		{"long line from the end", bashEvent("/", "tail -n 3 "+long),
			head + "One line in this range " + longLine + tail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runHookRaw(t, tc.ev); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
