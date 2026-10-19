// ctxguard: Claude Code status line and context-limit hooks.
//
//	ctxguard statusline   status line (stdin: status line JSON)
//	ctxguard hook         PostToolUse / Stop / SessionStart hook (stdin: hook JSON)
//
// Never fails loudly: any error exits 0 without output (statusline prints a
// dimmed "ctx –").
package main

import (
	"bytes"
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
	defaultMainLimit  = 150000
	handoffMaxAge     = 3 * time.Hour
	handoffMaxChars   = 9500

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
	case ctx >= 150000:
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
	Cwd            string `json:"cwd"`
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
	}
	if resp != nil {
		writeJSON(out, resp)
	}
}

func postToolUse(h hookInput) (any, string) {
	if h.AgentID == "" {
		return nil, "main-thread"
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
	return map[string]any{
		"decision": "block",
		"reason": fmt.Sprintf("Context is %dk (limit %dk). If agents or background work are still running, tell the user what is pending and stop. "+
			"Otherwise run the handoff command now (/handoff:handoff from the plugin, or /handoff), then stop.", n/1000, limit/1000),
	}, fmt.Sprintf("main-blocked %d", n)
}

func sessionStart(h hookInput) (any, string) {
	if h.Source != "compact" {
		return nil, "not-compact"
	}
	if safeName(h.SessionID) {
		if dir, err := stateDir(); err == nil {
			_ = os.Remove(filepath.Join(dir, "main-"+h.SessionID))
		}
	}
	if h.Cwd == "" {
		return nil, "no-cwd"
	}
	path, ok := newestHandoff(filepath.Join(h.Cwd, ".claude", "handoffs"), time.Now())
	if !ok {
		return nil, "no-handoff"
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, "handoff-unreadable"
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart",
		"additionalContext": "Handoff written before this compaction (" + path + ") — treat it as the authoritative state:\n\n" +
			truncateRunes(string(content), handoffMaxChars),
	}}, "handoff " + path
}

func newestHandoff(dir string, now time.Time) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, "agent-") || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime()
		if now.Sub(mod) > handoffMaxAge {
			continue
		}
		if best == "" || mod.After(bestMod) {
			best, bestMod = filepath.Join(dir, name), mod
		}
	}
	return best, best != ""
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

func writeJSON(out io.Writer, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		out.Write(b.Bytes())
	}
}

func logDecision(raw []byte, decision string) {
	path := os.Getenv("CTXGUARD_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s decision=%s\n%s\n", time.Now().Format(time.RFC3339), decision, bytes.TrimSpace(raw))
}
