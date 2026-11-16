package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func asst(tokens int, sidechain bool) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":%t,"message":{"role":"assistant","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":999}}}`,
		sidechain, tokens/2, tokens-tokens/2-10, 10)
}

func userLine(pad int) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, strings.Repeat("x", pad))
}

func writeLines(t *testing.T, path string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// padLines returns user lines totalling roughly n bytes.
func padLines(n int) []string {
	var out []string
	for n > 0 {
		out = append(out, userLine(64<<10))
		n -= 64 << 10
	}
	return out
}

func TestTranscriptContext(t *testing.T) {
	cases := []struct {
		name          string
		lines         []string
		skipSidechain bool
		want          int
		ok            bool
	}{
		{"last assistant wins", []string{asst(1000, false), userLine(10), asst(2000, false), userLine(10)}, true, 2000, true},
		{"sidechain skipped for main", []string{asst(1000, false), asst(5000, true)}, true, 1000, true},
		{"sidechain kept for agent", []string{asst(1000, true), asst(5000, true)}, false, 5000, true},
		{"assistant without usage skipped", []string{asst(1000, false), `{"type":"assistant","message":{"role":"assistant"}}`}, true, 1000, true},
		{"empty usage skipped", []string{asst(1000, false), `{"type":"assistant","message":{"usage":{}}}`}, true, 1000, true},
		{"garbage lines ignored", []string{asst(3000, false), `{"usage": not json`, ``}, true, 3000, true},
		{"no match", []string{userLine(10)}, true, 0, false},
		{"tail retry finds record past 512KB", append([]string{asst(7000, false)}, padLines(700<<10)...), true, 7000, true},
		{"beyond 4MB not found", append([]string{asst(7000, false)}, padLines(4200<<10)...), true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeLines(t, filepath.Join(t.TempDir(), "t.jsonl"), tc.lines...)
			got, ok := transcriptContext(p, tc.skipSidechain)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got (%d,%v) want (%d,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := transcriptContext(filepath.Join(t.TempDir(), "missing"), true); ok {
		t.Fatal("missing file should be unknown")
	}
}

func TestStatusline(t *testing.T) {
	dir := t.TempDir()
	tr := writeLines(t, filepath.Join(dir, "main.jsonl"), asst(110500, false), asst(9000, true))
	lim := fmt.Sprint(defaultMainLimit)
	limK := fmt.Sprint(defaultMainLimit / 1000)
	below := fmt.Sprint(defaultMainLimit - 1)
	cases := []struct {
		name, in, want string
	}{
		{"green from current_usage", `{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/a/proj/"},"context_window":{"current_usage":{"input_tokens":5,"cache_read_input_tokens":42000}}}`,
			"Opus · proj · " + green + "ctx 42k" + reset},
		{"red from transcript", `{"model":{"display_name":"Opus"},"cwd":"/x/y","transcript_path":"` + tr + `"}`,
			"Opus · y · " + red + "ctx 110k · handoff" + reset},
		{"green just below main limit", `{"cwd":"/x/y","context_window":{"current_usage":{"input_tokens":` + below + `}}}`,
			"y · " + green + "ctx " + fmt.Sprint((defaultMainLimit-1)/1000) + "k" + reset},
		{"red handoff at main limit", `{"model":{"display_name":"Opus"},"cwd":"/x/y","context_window":{"current_usage":{"input_tokens":` + lim + `}}}`,
			"Opus · y · " + red + "ctx " + limK + "k · handoff" + reset},
		{"zero current_usage falls back to transcript", `{"cwd":"/x/y","transcript_path":"` + tr + `","context_window":{"current_usage":{"input_tokens":0}}}`,
			"y · " + red + "ctx 110k · handoff" + reset},
		{"workspace preferred over cwd", `{"workspace":{"current_dir":"/w/one"},"cwd":"/x/two"}`,
			"one · " + dim + "ctx –" + reset},
		{"unknown", `{"model":{"display_name":"Opus"},"cwd":"/x/y","transcript_path":"/nope"}`,
			"Opus · y · " + dim + "ctx –" + reset},
		{"bad json", `not json`, dim + "ctx –" + reset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			statusline(strings.NewReader(tc.in), &out)
			if got := out.String(); got != tc.want+"\n" {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func runHook(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	hook(bytes.NewReader(b), &out)
	if out.Len() == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("bad output %q: %v", out.String(), err)
	}
	return m
}

func setup(t *testing.T) (state string) {
	t.Helper()
	state = filepath.Join(t.TempDir(), "state")
	t.Setenv("CTXGUARD_STATE_DIR", state)
	t.Setenv("CTXGUARD_AGENT_LIMIT", "")
	t.Setenv("CTXGUARD_MAIN_LIMIT", "")
	t.Setenv("CTXGUARD_LOG", "")
	t.Setenv("CTXGUARD_REAP", "")
	t.Setenv("CTXGUARD_REAP_IDLE", "")
	// Tests never list or signal real processes.
	ps, sig := psSnapshot, signalTerm
	t.Cleanup(func() { psSnapshot, signalTerm = ps, sig })
	psSnapshot = func() (string, error) { return "", nil }
	signalTerm = func(pid int) error { t.Fatalf("real signal to %d", pid); return nil }
	return state
}

func additional(m map[string]any) string {
	if m == nil {
		return ""
	}
	h, _ := m["hookSpecificOutput"].(map[string]any)
	s, _ := h["additionalContext"].(string)
	return s
}

func TestPostToolUse(t *testing.T) {
	state := setup(t)
	proj := t.TempDir()
	mainTr := writeLines(t, filepath.Join(proj, "sess.jsonl"), asst(10000, false))
	writeLines(t, filepath.Join(proj, "sess", "subagents", "agent-low.jsonl"), asst(119999, true))
	highTr := writeLines(t, filepath.Join(proj, "sess", "subagents", "agent-high.jsonl"), asst(130400, true))
	writeLines(t, filepath.Join(proj, "sess", "subagents", "agent-env.jsonl"), asst(5000, true))

	ev := func(agent, tr string) map[string]any {
		return map[string]any{"hook_event_name": "PostToolUse", "session_id": "sess", "transcript_path": tr, "agent_id": agent,
			"tool_name": "Read", "tool_response": map[string]any{"x": strings.Repeat("y", 1000)}}
	}
	cases := []struct {
		name, agent, tr, env string
		wantMsg              bool
	}{
		{"no agent_id silent", "", mainTr, "", false},
		{"agent below limit", "low", mainTr, "", false},
		{"agent above limit", "high", mainTr, "", true},
		{"agent above limit only once", "high", mainTr, "", false},
		{"missing agent transcript", "ghost", mainTr, "", false},
		{"path-like agent id rejected", "../sess", mainTr, "", false},
		{"transcript_path inside subagents dir", "env", highTr, "1000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CTXGUARD_AGENT_LIMIT", tc.env)
			got := additional(runHook(t, ev(tc.agent, tc.tr)))
			if tc.wantMsg != (got != "") {
				t.Fatalf("wantMsg=%v got %q", tc.wantMsg, got)
			}
			if tc.wantMsg && !strings.HasPrefix(got, "CONTEXT LIMIT: you are at ") {
				t.Fatalf("unexpected message %q", got)
			}
		})
	}
	if got := additional(runHook(t, ev("high", mainTr))); got != "" {
		t.Fatalf("second notice: %q", got)
	}
	if _, err := os.Stat(filepath.Join(state, "agent-high")); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	// Exact message for the 130k agent.
	os.Remove(filepath.Join(state, "agent-high"))
	want := "CONTEXT LIMIT: you are at 130k tokens. Finish the step you are on (do not start a new one), then hand off as your agent instructions say: write the handoff file and return Status: HANDOFF <path>."
	m := runHook(t, ev("high", mainTr))
	if got := additional(m); got != want {
		t.Fatalf("got %q", got)
	}
	if m["hookSpecificOutput"].(map[string]any)["hookEventName"] != "PostToolUse" {
		t.Fatalf("bad event name: %v", m)
	}
}

func TestStop(t *testing.T) {
	state := setup(t)
	dir := t.TempDir()
	low := writeLines(t, filepath.Join(dir, "low.jsonl"), asst(defaultMainLimit-1, false), asst(200000, true))
	high := writeLines(t, filepath.Join(dir, "high.jsonl"), asst(120400, false))
	ev := func(sess, tr string, active bool) map[string]any {
		return map[string]any{"hook_event_name": "Stop", "session_id": sess, "transcript_path": tr, "stop_hook_active": active}
	}
	wantReason := "Context is 120k (limit 100k). Write or refresh the handoff file now (the handoff command, steps 1–4), " +
		"listing running agents/background work as in flight with their output files. " +
		"Do not stop, do not wait for them, and do not ask the user to open a new chat: " +
		"then continue with Next in this chat — auto-compaction will re-inject the handoff." +
		"\nNo handoff recorded for this session: write it with the Write tool (a Bash-written file is not re-injected)."
	cases := []struct {
		name, sess, tr string
		active, block  bool
	}{
		{"below limit (sidechain ignored)", "s1", low, false, false},
		{"stop_hook_active", "s2", high, true, false},
		{"above limit blocks", "s2", high, false, true},
		{"only once per session", "s2", high, false, false},
		{"other session blocks", "s3", high, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := runHook(t, ev(tc.sess, tc.tr, tc.active))
			if !tc.block {
				if m != nil {
					t.Fatalf("unexpected output %v", m)
				}
				return
			}
			if m["decision"] != "block" || m["reason"] != wantReason {
				t.Fatalf("got %v", m)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(state, "main-s2")); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
}

func launch(id string, sidechain bool) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":%t,"message":{"role":"user","content":[{"type":"tool_result","content":"Async agent launched successfully."}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":%q,"description":"do %s"}}`, sidechain, id, id)
}

func notify(id, typ string) string {
	return fmt.Sprintf(`{"type":%q,"isSidechain":false,"content":"<task-notification>\n<task-id>%s</task-id>\n<status>completed</status>\n</task-notification>"}`, typ, id)
}

func TestRunningAgents(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"none", []string{asst(1000, false)}, ""},
		{"launched, not notified", []string{launch("a1", false), asst(1000, false)}, "a1:do a1"},
		{"notified", []string{launch("a1", false), notify("a1", "attachment")}, ""},
		{"queued only still running", []string{launch("a1", false), notify("a1", "queue-operation")}, "a1:do a1"},
		{"sidechain launch ignored", []string{launch("a1", true)}, ""},
		{"one of two done", []string{launch("a1", false), launch("a2", false), notify("a1", "attachment")}, "a2:do a2"},
		{"launch far behind the tail", append([]string{launch("a1", false)}, padLines(4200<<10)...), "a1:do a1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeLines(t, filepath.Join(t.TempDir(), "t.jsonl"), tc.lines...)
			agents, err := runningAgents(p)
			var got []string
			for _, a := range agents {
				got = append(got, a.ID+":"+a.Description)
			}
			if err != nil || strings.Join(got, ",") != tc.want {
				t.Fatalf("got (%v,%v) want %q", agents, err, tc.want)
			}
		})
	}
}

func TestRunningAgentString(t *testing.T) {
	if got := (runningAgent{ID: "a1"}).String(); got != "a1" {
		t.Fatalf("empty description: got %q", got)
	}
	if got := (runningAgent{ID: "a1", Description: "do a1"}).String(); got != "a1 (do a1)" {
		t.Fatalf("got %q", got)
	}
}

func TestStopListsRunningAgents(t *testing.T) {
	setup(t)
	tr := writeLines(t, filepath.Join(t.TempDir(), "t.jsonl"),
		launch("a1", false), launch("a2", false), launch("a3", false), notify("a2", "attachment"), asst(120400, false))
	m := runHook(t, map[string]any{"hook_event_name": "Stop", "session_id": "sl", "transcript_path": tr})
	reason, _ := m["reason"].(string)
	want := " Running agents (launched, no result yet): a1 (do a1), a3 (do a3).\n"
	if m["decision"] != "block" || !strings.Contains(reason, want) {
		t.Fatalf("running agents should be listed after the reason: %v", m)
	}
}

func TestStopNamesRecordedHandoff(t *testing.T) {
	cases := []struct {
		name   string
		record bool
		want   func(path string) string
	}{
		{"recorded handoff", true, func(path string) string { return "\nRecorded handoff: " + path }},
		{"no recorded handoff", false, func(string) string {
			return "\nNo handoff recorded for this session: write it with the Write tool (a Bash-written file is not re-injected)."
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			cwd := t.TempDir()
			path := writeLines(t, filepath.Join(cwd, ".claude", "handoffs", "main-own.md"), "own")
			if tc.record {
				runHook(t, writeEvent("sh", cwd, "Write", path))
			}
			tr := writeLines(t, filepath.Join(t.TempDir(), "t.jsonl"), asst(120400, false))
			m := runHook(t, map[string]any{"hook_event_name": "Stop", "session_id": "sh", "transcript_path": tr})
			reason, _ := m["reason"].(string)
			if m["decision"] != "block" || !strings.HasSuffix(reason, tc.want(path)) {
				t.Fatalf("want reason ending %q, got %v", tc.want(path), m)
			}
		})
	}
}

// observedCompactAt was measured 2026-10-20 from ~/.claude/state/ctxguard/log.jsonl:
// compaction fires after Stop readings of ~129–137k under autoCompactWindow 170000.
const observedCompactAt = 137000

func TestMainLimitBelowObservedCompaction(t *testing.T) {
	if defaultMainLimit+15000 > observedCompactAt {
		t.Fatalf("defaultMainLimit %d leaves less than 15k headroom below the observed compaction point %d; "+
			"re-measure where compaction actually fires (log.jsonl Stop readings) before raising the limit",
			defaultMainLimit, observedCompactAt)
	}
}

func TestStopBlocksAgainAfterCompaction(t *testing.T) {
	state := setup(t)
	dir := t.TempDir()
	tr := writeLines(t, filepath.Join(dir, "at.jsonl"), asst(defaultMainLimit, false))
	stopEv := map[string]any{"hook_event_name": "Stop", "session_id": "sx", "transcript_path": tr}
	compactEv := map[string]any{"hook_event_name": "SessionStart", "source": "compact", "session_id": "sx", "cwd": t.TempDir()}

	m := runHook(t, stopEv)
	if m["decision"] != "block" {
		t.Fatalf("at limit should block: %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.Contains(reason, "handoff command") || !strings.Contains(reason, "continue with Next in this chat") {
		t.Fatalf("reason should ask for a handoff and to keep working: %q", reason)
	}
	for _, bad := range []string{"tell the user what is pending and stop", "Then stop."} {
		if strings.Contains(reason, bad) {
			t.Fatalf("reason still says %q: %q", bad, reason)
		}
	}
	if m := runHook(t, stopEv); m != nil {
		t.Fatalf("second stop should not block (marker): %v", m)
	}
	if m := runHook(t, compactEv); m != nil {
		t.Fatalf("compact without handoff should inject nothing: %v", m)
	}
	if _, err := os.Stat(filepath.Join(state, "main-sx")); !os.IsNotExist(err) {
		t.Fatalf("marker not removed on compact without handoff: %v", err)
	}
	if m := runHook(t, stopEv); m["decision"] != "block" {
		t.Fatalf("stop after compaction should block again: %v", m)
	}
	// Compact with no marker present is harmless.
	os.Remove(filepath.Join(state, "main-sx"))
	runHook(t, compactEv)
}

func TestSessionStartCompact(t *testing.T) {
	testSessionStart(t, "compact", "Handoff written before this compaction (")
}

func TestSessionStartClear(t *testing.T) {
	testSessionStart(t, "clear", "Handoff written before /clear (")
}

func TestStopBlocksAgainAfterClear(t *testing.T) {
	state := setup(t)
	tr := writeLines(t, filepath.Join(t.TempDir(), "at.jsonl"), asst(defaultMainLimit, false))
	stopEv := map[string]any{"hook_event_name": "Stop", "session_id": "sc", "transcript_path": tr}
	if m := runHook(t, stopEv); m["decision"] != "block" {
		t.Fatalf("at limit should block: %v", m)
	}
	runHook(t, map[string]any{"hook_event_name": "SessionStart", "source": "clear", "session_id": "sc", "cwd": t.TempDir()})
	if _, err := os.Stat(filepath.Join(state, "main-sc")); !os.IsNotExist(err) {
		t.Fatalf("marker not removed on clear: %v", err)
	}
	if m := runHook(t, stopEv); m["decision"] != "block" {
		t.Fatalf("stop after clear should block again: %v", m)
	}
}

func testSessionStart(t *testing.T, source, header string) {
	state := setup(t)
	cwd := t.TempDir()
	hd := filepath.Join(cwd, ".claude", "handoffs")
	now := time.Now()
	write := func(name, content string, age time.Duration) string {
		p := writeLines(t, filepath.Join(hd, name), content)
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("old.md", "old", 4*time.Hour)
	write("agent-x-1.md", "agent newest", 0)
	want := write("main-2.md", strings.Repeat("é", 10000), 10*time.Minute)
	other := write("main-1.md", "another chat", time.Minute)
	runHook(t, writeEvent("sess", cwd, "Write", want))
	runHook(t, writeEvent("other", cwd, "Write", other))

	started := "sess"
	if source == "clear" {
		runHook(t, endEvent("sess", "clear", cwd))
		started = "fresh"
	}
	os.MkdirAll(state, 0o755)
	marker := filepath.Join(state, "main-"+started)
	os.WriteFile(marker, nil, 0o644)

	ev := map[string]any{"hook_event_name": "SessionStart", "source": source, "session_id": started, "cwd": cwd}
	m := runHook(t, ev)
	got := additional(m)
	prefix := header + want + ") — treat it as the authoritative state:\n\n"
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("got prefix %q", got[:min(len(got), 200)])
	}
	if body := strings.TrimPrefix(got, prefix); body != strings.Repeat("é", 9500) {
		t.Fatalf("body not truncated to 9500 chars: %d runes", len([]rune(body)))
	}
	if m["hookSpecificOutput"].(map[string]any)["hookEventName"] != "SessionStart" {
		t.Fatalf("bad event name %v", m)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker not reset: %v", err)
	}

	tr := writeLines(t, filepath.Join(t.TempDir(), "t.jsonl"), launch("a1", false))
	cases := []struct {
		name string
		ev   map[string]any
	}{
		{"startup ignored", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "sess", "cwd": cwd, "transcript_path": tr}},
		{"resume ignored", map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "sess", "cwd": cwd, "transcript_path": tr}},
		{"no session handoff", map[string]any{"hook_event_name": "SessionStart", "source": source, "session_id": "nobody", "cwd": t.TempDir()}},
		{"other event", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "sess", "cwd": cwd}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if m := runHook(t, tc.ev); m != nil {
				t.Fatalf("unexpected output %v", m)
			}
		})
	}
}

func writeEvent(session, cwd, tool, path string) map[string]any {
	return map[string]any{"hook_event_name": "PostToolUse", "session_id": session, "cwd": cwd,
		"tool_name": tool, "tool_input": map[string]any{"file_path": path, "content": "x"}}
}

func endEvent(session, reason, cwd string) map[string]any {
	return map[string]any{"hook_event_name": "SessionEnd", "session_id": session, "reason": reason, "cwd": cwd}
}

func TestSessionHandoffOwnership(t *testing.T) {
	type write struct{ session, tool, file string }
	cases := []struct {
		name      string
		writes    []write
		end       []string // session, reason of a SessionEnd in cwd
		source    string
		session   string
		elsewhere bool // start in another cwd
		want      string
	}{
		{"own handoff beats a newer one from another chat", []write{{"sess", "Write", "own"}, {"other", "Write", "other"}}, nil, "compact", "sess", false, "own"},
		{"no record means no handoff", []write{{"other", "Write", "other"}}, nil, "compact", "sess", false, ""},
		{"agent handoff never recorded", []write{{"sess", "Write", "agent"}}, nil, "compact", "sess", false, ""},
		{"write outside the handoffs dir not recorded", []write{{"sess", "Write", "outside"}, {"sess", "Write", "nested"}}, nil, "compact", "sess", false, ""},
		{"read is not recorded", []write{{"sess", "Read", "own"}}, nil, "compact", "sess", false, ""},
		{"edit records", []write{{"sess", "Edit", "own"}}, nil, "compact", "sess", false, "own"},
		{"latest write wins", []write{{"sess", "Write", "other"}, {"sess", "Edit", "own"}}, nil, "compact", "sess", false, "own"},
		{"stale own handoff skipped", []write{{"sess", "Write", "stale"}}, nil, "compact", "sess", false, ""},
		{"/clear picks the cleared session's handoff", []write{{"sess", "Write", "own"}, {"other", "Write", "other"}}, []string{"sess", "clear"}, "clear", "fresh", false, "own"},
		{"/clear without a cleared session", []write{{"sess", "Write", "own"}}, nil, "clear", "fresh", false, ""},
		{"session end for another reason", []write{{"sess", "Write", "own"}}, []string{"sess", "logout"}, "clear", "fresh", false, ""},
		{"/clear in another cwd", []write{{"sess", "Write", "own"}}, []string{"sess", "clear"}, "clear", "fresh", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			cwd := t.TempDir()
			hd := filepath.Join(cwd, ".claude", "handoffs")
			now := time.Now()
			files := map[string]string{}
			for name, f := range map[string]struct {
				path string
				age  time.Duration
			}{
				"own":     {filepath.Join(hd, "main-own.md"), 10 * time.Minute},
				"other":   {filepath.Join(hd, "main-other.md"), time.Minute},
				"agent":   {filepath.Join(hd, "agent-sess-1.md"), 0},
				"stale":   {filepath.Join(hd, "main-stale.md"), 4 * time.Hour},
				"outside": {filepath.Join(cwd, "notes.md"), 0},
				"nested":  {filepath.Join(hd, "sub", "main-x.md"), 0},
			} {
				files[name] = writeLines(t, f.path, name)
				if err := os.Chtimes(f.path, now.Add(-f.age), now.Add(-f.age)); err != nil {
					t.Fatal(err)
				}
			}
			for _, w := range tc.writes {
				runHook(t, writeEvent(w.session, cwd, w.tool, files[w.file]))
			}
			if tc.end != nil {
				runHook(t, endEvent(tc.end[0], tc.end[1], cwd))
			}
			startCwd := cwd
			if tc.elsewhere {
				startCwd = t.TempDir()
			}
			got := additional(runHook(t, map[string]any{"hook_event_name": "SessionStart", "source": tc.source, "session_id": tc.session, "cwd": startCwd}))
			if tc.want == "" {
				if got != "" {
					t.Fatalf("want no handoff, got %q", got)
				}
				return
			}
			if !strings.Contains(got, "("+files[tc.want]+")") || !strings.HasSuffix(got, "\n\n"+tc.want+"\n") {
				t.Fatalf("want %s, got %q", files[tc.want], got)
			}
		})
	}
}

func TestClearMappingUsedOnce(t *testing.T) {
	setup(t)
	cwd := t.TempDir()
	hp := writeLines(t, filepath.Join(cwd, ".claude", "handoffs", "main-1.md"), "state")
	runHook(t, writeEvent("sess", cwd, "Write", hp))
	runHook(t, endEvent("sess", "clear", cwd))
	start := map[string]any{"hook_event_name": "SessionStart", "source": "clear", "session_id": "fresh", "cwd": cwd}
	if got := additional(runHook(t, start)); !strings.Contains(got, hp) {
		t.Fatalf("first /clear start should inject %s: %q", hp, got)
	}
	if m := runHook(t, start); m != nil {
		t.Fatalf("mapping should be removed after use: %v", m)
	}
}

func TestSessionStartRunningAgents(t *testing.T) {
	setup(t)
	cwd := t.TempDir()
	hp := writeLines(t, filepath.Join(cwd, ".claude", "handoffs", "main-1.md"), "state")
	runHook(t, writeEvent("sr", cwd, "Write", hp))
	tr := writeLines(t, filepath.Join(t.TempDir(), "t.jsonl"), launch("a1", false), launch("a2", false), notify("a1", "attachment"))
	ev := func(source, session string) map[string]any {
		return map[string]any{"hook_event_name": "SessionStart", "source": source, "session_id": session, "cwd": cwd, "transcript_path": tr}
	}

	got := additional(runHook(t, ev("compact", "sr")))
	want := "Handoff written before this compaction (" + hp + ") — treat it as the authoritative state:\n\nstate\n" +
		"\n\nStill running from before this compaction (launched, no result yet; their notifications still arrive):\n- a2 (do a2)"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}

	got = additional(runHook(t, ev("compact", "no-handoff")))
	want = "Still running from before this compaction (launched, no result yet; their notifications still arrive):\n- a2 (do a2)"
	if got != want {
		t.Fatalf("without a handoff, only the list: got %q", got)
	}

	runHook(t, endEvent("sr", "clear", cwd))
	got = additional(runHook(t, ev("clear", "fresh")))
	want = "Handoff written before /clear (" + hp + ") — treat it as the authoritative state:\n\nstate\n"
	if got != want {
		t.Fatalf("/clear injects the handoff without a list: got %q", got)
	}
	if m := runHook(t, ev("clear", "fresh")); m != nil {
		t.Fatalf("a second /clear start has no cleared session to load: %v", m)
	}

	var lines []string
	for i := range 23 {
		lines = append(lines, launch(fmt.Sprintf("b%02d", i), false))
	}
	writeLines(t, tr, lines...)
	got = additional(runHook(t, ev("compact", "no-handoff")))
	if !strings.HasSuffix(got, "\n- b19 (do b19)\n… and 3 more") || strings.Contains(got, "b20") {
		t.Fatalf("list should stop at 20 entries: %q", got)
	}
}

func TestHookLogAndBadInput(t *testing.T) {
	setup(t)
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("CTXGUARD_LOG", log)
	var out bytes.Buffer
	hook(strings.NewReader("not json"), &out)
	hook(strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"s"}`), &out)
	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
	b, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(b), "decision=bad-json") || !strings.Contains(string(b), "decision=main-thread") {
		t.Fatalf("log %q err %v", b, err)
	}
}
