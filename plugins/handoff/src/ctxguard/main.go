// ctxguard: Claude Code status line and context-limit hooks.
//
//	ctxguard statusline   status line (stdin: status line JSON)
//	ctxguard hook         PostToolUse / Stop / SessionStart / SessionEnd hook (stdin: hook JSON)
//	ctxguard reap [--dry-run]   SIGTERM orphaned processes of idle sessions
//
// The hooks never fail loudly: any error exits 0 without output (statusline
// prints a dimmed "ctx –").
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAgentLimit = 120000
	defaultMainLimit  = 100000 // well under the observed compaction point; see TestMainLimitBelowObservedCompaction
	handoffMaxAge     = 3 * time.Hour
	handoffMaxChars   = 9500
	maxListedAgents   = 20

	// State files: a session's recorded handoff path, and the session /clear
	// ended in a cwd. The reaper prunes both once idle.
	handoffStatePrefix = "handoff-"
	clearStatePrefix   = "clear-"

	green  = "\033[32m"
	yellow = "\033[33m"
	red    = "\033[31m"
	dim    = "\033[2m"
	reset  = "\033[0m"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "statusline":
		defer func() {
			if recover() != nil {
				fmt.Println(dim + "ctx –" + reset)
			}
		}()
		statusline(os.Stdin, os.Stdout)
	case "hook":
		defer func() { recover() }()
		hook(os.Stdin, os.Stdout)
	case "reap":
		os.Exit(reapMain(os.Args[2:], os.Stdout, os.Stderr))
	}
}

// ---- statusline ----

type statusInput struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	ContextWindow  struct {
		CurrentUsage map[string]any `json:"current_usage"`
	} `json:"context_window"`
}

func statusline(in io.Reader, out io.Writer) {
	var d statusInput
	if raw, err := io.ReadAll(in); err == nil {
		_ = json.Unmarshal(raw, &d) // partial results are fine
	}
	ctx, known := 0, false
	if d.ContextWindow.CurrentUsage != nil {
		if n := usageTokens(d.ContextWindow.CurrentUsage); n != 0 {
			ctx, known = n, true
		}
	}
	if !known && d.TranscriptPath != "" {
		ctx, known = transcriptContext(d.TranscriptPath, true)
	}
	var c string
	switch {
	case !known:
		c = dim + "ctx –" + reset
	case ctx >= defaultMainLimit:
		c = fmt.Sprintf("%sctx %dk · handoff%s", red, ctx/1000, reset)
	case ctx >= 100000:
		c = fmt.Sprintf("%sctx %dk%s", yellow, ctx/1000, reset)
	default:
		c = fmt.Sprintf("%sctx %dk%s", green, ctx/1000, reset)
	}
	cwd := d.Workspace.CurrentDir
	if cwd == "" {
		cwd = d.Cwd
	}
	var parts []string
	for _, p := range []string{d.Model.DisplayName, basename(cwd), c} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	fmt.Fprintln(out, strings.Join(parts, " · "))
}

// basename mirrors Python's os.path.basename(p.rstrip("/")).
func basename(p string) string {
	p = strings.TrimRight(p, "/")
	return p[strings.LastIndexByte(p, '/')+1:]
}

// ---- hook ----

type hookInput struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	AgentID        string `json:"agent_id"`
	StopHookActive bool   `json:"stop_hook_active"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
	Cwd            string `json:"cwd"`
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

func hook(in io.Reader, out io.Writer) {
	raw, err := io.ReadAll(in)
	if err != nil {
		return
	}
	decision := "none"
	defer func() { logDecision(raw, decision) }()

	var h hookInput
	if err := json.Unmarshal(raw, &h); err != nil {
		decision = "bad-json"
		return
	}
	var resp any
	switch h.HookEventName {
	case "PostToolUse":
		resp, decision = postToolUse(h)
	case "Stop":
		resp, decision = stop(h)
	case "SessionStart":
		resp, decision = sessionStart(h)
	case "SessionEnd":
		decision = sessionEnd(h)
	}
	if resp != nil {
		writeJSON(out, resp)
	}
	if h.HookEventName == "SessionStart" {
		reapOnSessionStart(h.SessionID)
	}
}

func postToolUse(h hookInput) (any, string) {
	if h.AgentID == "" {
		return nil, recordHandoff(h)
	}
	if !safeName(h.AgentID) || h.TranscriptPath == "" {
		return nil, "bad-input"
	}
	dir := filepath.Dir(h.TranscriptPath)
	var path string
	if filepath.Base(dir) == "subagents" {
		path = filepath.Join(dir, "agent-"+h.AgentID+".jsonl")
	} else {
		if !safeName(h.SessionID) {
			return nil, "bad-input"
		}
		path = filepath.Join(dir, h.SessionID, "subagents", "agent-"+h.AgentID+".jsonl")
	}
	n, ok := transcriptContext(path, false)
	if !ok {
		return nil, "agent-unknown " + path
	}
	limit := envInt("CTXGUARD_AGENT_LIMIT", defaultAgentLimit)
	if n < limit {
		return nil, fmt.Sprintf("agent-below %d", n)
	}
	if !createMarker("agent-" + h.AgentID) {
		return nil, fmt.Sprintf("agent-already-told %d", n)
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PostToolUse",
		"additionalContext": fmt.Sprintf("CONTEXT LIMIT: you are at %dk tokens. Finish the step you are on (do not start a new one), "+
			"then hand off as your agent instructions say: write the handoff file and return Status: HANDOFF <path>.", n/1000),
	}}, fmt.Sprintf("agent-notified %d", n)
}

// recordHandoff remembers a main-thread Write or Edit of a handoff file as the
// session's own handoff: chats sharing a cwd must never load each other's.
func recordHandoff(h hookInput) string {
	if h.ToolName != "Write" && h.ToolName != "Edit" {
		return "main-thread"
	}
	path := h.ToolInput.FilePath
	if h.Cwd == "" || filepath.Dir(path) != filepath.Join(h.Cwd, ".claude", "handoffs") ||
		strings.HasPrefix(filepath.Base(path), "agent-") {
		return "main-thread"
	}
	if !safeName(h.SessionID) {
		return "bad-input"
	}
	if !writeState(handoffStatePrefix+h.SessionID, path) {
		return "handoff-unrecorded " + path
	}
	return "handoff-recorded " + path
}

// sessionEnd remembers which session /clear ended in a cwd, so the session
// that replaces it there loads that session's handoff.
func sessionEnd(h hookInput) string {
	if h.Reason != "clear" {
		return "not-clear"
	}
	if h.Cwd == "" || !safeName(h.SessionID) {
		return "bad-input"
	}
	if !writeState(clearStatePrefix+cwdKey(h.Cwd), h.SessionID) {
		return "clear-unrecorded"
	}
	return "clear-recorded " + h.SessionID
}

func stop(h hookInput) (any, string) {
	if h.StopHookActive {
		return nil, "stop-hook-active"
	}
	if h.TranscriptPath == "" || !safeName(h.SessionID) {
		return nil, "bad-input"
	}
	n, ok := transcriptContext(h.TranscriptPath, true)
	if !ok {
		return nil, "main-unknown"
	}
	limit := envInt("CTXGUARD_MAIN_LIMIT", defaultMainLimit)
	if n < limit {
		return nil, fmt.Sprintf("main-below %d", n)
	}
	if !createMarker("main-" + h.SessionID) {
		return nil, fmt.Sprintf("main-already-blocked %d", n)
	}
	reason := fmt.Sprintf("Context is %dk (limit %dk). Write or refresh the handoff file now (the handoff command, steps 1–4), "+
		"listing running agents/background work as in flight with their output files. "+
		"Do not stop, do not wait for them, and do not ask the user to open a new chat: "+
		"then continue with Next in this chat — auto-compaction will re-inject the handoff.", n/1000, limit/1000)
	decision := fmt.Sprintf("main-blocked %d", n)
	if agents, err := runningAgents(h.TranscriptPath); err == nil && len(agents) > 0 {
		names := make([]string, len(agents))
		for i, a := range agents {
			names[i] = a.String()
		}
		reason += " Running agents (launched, no result yet): " + strings.Join(names, ", ") + "."
		decision += fmt.Sprintf(" running=%d", len(agents))
	}
	// Only a main-thread Write or Edit records the handoff; a Bash-written
	// file is silently not re-injected.
	if path, _ := sessionHandoff(h); path != "" {
		reason += "\nRecorded handoff: " + path
	} else {
		reason += "\nNo handoff recorded for this session: write it with the Write tool (a Bash-written file is not re-injected)."
	}
	return map[string]any{"decision": "block", "reason": reason}, decision
}

func sessionStart(h hookInput) (any, string) {
	var before string
	switch h.Source {
	case "compact":
		before = "this compaction"
	case "clear":
		before = "/clear"
	default:
		return nil, "not-compact"
	}
	// Reset the once-per-session Stop marker on every compaction or /clear
	// (handoff or not), so the guard can block again before the next one.
	if safeName(h.SessionID) {
		if dir, err := stateDir(); err == nil {
			_ = os.Remove(filepath.Join(dir, "main-"+h.SessionID)) // not-exist is fine
		}
	}
	var sections []string
	path, decision := sessionHandoff(h)
	if path != "" {
		var section string
		section, decision = handoffSection(path, before)
		if section != "" {
			sections = append(sections, section)
		}
	}
	// The transcript outlives a compaction, so launches from before it are still
	// there; /clear starts a new transcript, so it has none to list.
	if h.Source == "compact" {
		if agents, err := runningAgents(h.TranscriptPath); err == nil && len(agents) > 0 {
			sections = append(sections, runningSection(agents))
			decision += fmt.Sprintf(" running=%d", len(agents))
		}
	}
	if len(sections) == 0 {
		return nil, decision
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SessionStart",
		"additionalContext": strings.Join(sections, "\n\n"),
	}}, decision
}

// sessionHandoff returns the handoff recorded for the session being continued:
// the compacted one, or after /clear the one /clear ended in this cwd. Without
// a record there is no handoff; another chat's file is never a fallback.
func sessionHandoff(h hookInput) (string, string) {
	owner := h.SessionID
	if h.Source == "clear" {
		var ok bool
		if owner, ok = takeClearedSession(h.Cwd); !ok {
			return "", "no-cleared-session"
		}
	}
	if !safeName(owner) {
		return "", "bad-input"
	}
	path, ok := readState(handoffStatePrefix + owner)
	if !ok {
		return "", "no-session-handoff"
	}
	return path, ""
}

// takeClearedSession returns the session /clear ended in cwd and forgets it.
func takeClearedSession(cwd string) (string, bool) {
	if cwd == "" {
		return "", false
	}
	name := clearStatePrefix + cwdKey(cwd)
	id, ok := readState(name)
	removeState(name)
	return id, ok && safeName(id)
}

// handoffSection returns the handoff at path as context ("" unless it is a
// regular file written within handoffMaxAge) and the decision to log.
func handoffSection(path, before string) (string, string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) > handoffMaxAge {
		return "", "stale-handoff " + path
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", "handoff-unreadable"
	}
	return "Handoff written before " + before + " (" + path + ") — treat it as the authoritative state:\n\n" +
		truncateRunes(string(content), handoffMaxChars), "handoff " + path
}

func runningSection(agents []runningAgent) string {
	var b strings.Builder
	b.WriteString("Still running from before this compaction (launched, no result yet; their notifications still arrive):")
	for _, a := range agents[:min(len(agents), maxListedAgents)] {
		b.WriteString("\n- " + a.String())
	}
	if extra := len(agents) - maxListedAgents; extra > 0 {
		fmt.Fprintf(&b, "\n… and %d more", extra)
	}
	return b.String()
}

func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// ---- helpers ----

func safeName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func stateDir() (string, error) {
	if d := os.Getenv("CTXGUARD_STATE_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "state", "ctxguard"), nil
}

// createMarker creates the marker atomically; false if it already existed or
// could not be created.
func createMarker(name string) bool {
	dir, err := stateDir()
	if err != nil || os.MkdirAll(dir, 0o755) != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	fmt.Fprintln(f, time.Now().Format(time.RFC3339))
	f.Close()
	return true
}

// writeState replaces the state file name with value; false if it could not.
func writeState(name, value string) bool {
	dir, err := stateDir()
	if err != nil || os.MkdirAll(dir, 0o755) != nil {
		return false
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644) == nil
}

func readState(name string) (string, bool) {
	dir, err := stateDir()
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	v := strings.TrimSpace(string(b))
	return v, err == nil && v != ""
}

func removeState(name string) {
	if dir, err := stateDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, name)) // not-exist is fine
	}
}

// cwdKey names a cwd in a state file name.
func cwdKey(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:16])
}

func writeJSON(out io.Writer, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		out.Write(b.Bytes())
	}
}

func logDecision(raw []byte, decision string) {
	appendLog("decision="+decision, bytes.TrimSpace(raw))
}

func appendLog(head string, body []byte) {
	path := os.Getenv("CTXGUARD_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n%s\n", time.Now().Format(time.RFC3339), head, body)
}
