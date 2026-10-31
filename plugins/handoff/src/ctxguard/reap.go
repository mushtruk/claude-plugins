package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Background processes started from a session's temp dir outlive the session
// when their shell exits. A process is reaped only when every check proves it
// is abandoned; anything unproven is skipped.

const defaultReapIdle = 72 * time.Hour

// claudeTempPath matches /private/tmp/claude-<uid>/<encoded-project>/<session-uuid>/.
var claudeTempPath = regexp.MustCompile(`(?:^|[\s='"])(?:/private)?/tmp/claude-(\d+)/([^/\s'"]+)/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/`)

// Seams: tests replace both so they never list or signal real processes.
var (
	psSnapshot = func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,uid=,etime=,command=").Output()
		return string(out), err
	}
	signalTerm = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
)

type proc struct {
	pid, ppid, uid int
	etime, command string
}

type verdict struct {
	PID       int    `json:"pid"`
	PPID      int    `json:"ppid"`
	Age       string `json:"age"`
	Session   string `json:"session"`
	Project   string `json:"project"`
	Idle      string `json:"transcript_idle"`
	Kill      bool   `json:"kill"`
	Reason    string `json:"reason"`
	Command   string `json:"command"`
	DryRun    bool   `json:"dry_run"`
	SignalErr string `json:"signal_error,omitempty"`
}

func (v verdict) label() string {
	if v.Kill {
		return "KILL"
	}
	return "SKIP"
}

type reaper struct {
	uid     int
	home    string
	self    string // session whose processes are never reaped
	idle    time.Duration
	now     func() time.Time
	modTime func(path string) (time.Time, error)
	signal  func(pid int) error
}

// newReaper reads CTXGUARD_REAP (0 disables) and CTXGUARD_REAP_IDLE.
func newReaper(self string) (reaper, bool, error) {
	if os.Getenv("CTXGUARD_REAP") == "0" {
		return reaper{}, false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return reaper{}, true, err
	}
	idle := defaultReapIdle
	if d, err := time.ParseDuration(os.Getenv("CTXGUARD_REAP_IDLE")); err == nil && d > 0 {
		idle = d
	}
	return reaper{
		uid:  os.Getuid(),
		home: home,
		self: self,
		idle: idle,
		now:  time.Now,
		modTime: func(p string) (time.Time, error) {
			st, err := os.Stat(p)
			if err != nil {
				return time.Time{}, err
			}
			return st.ModTime(), nil
		},
		signal: signalTerm,
	}, true, nil
}

// reap judges one ps snapshot; enabled is false when CTXGUARD_REAP=0.
func reap(self string, dryRun bool) (vs []verdict, enabled bool, err error) {
	r, enabled, err := newReaper(self)
	if !enabled || err != nil {
		return nil, enabled, err
	}
	snap, err := psSnapshot()
	if err != nil {
		return nil, true, err
	}
	return r.run(snap, dryRun), true, nil
}

// run returns a verdict for every process whose command line holds a Claude
// temp path, signalling the KILL ones unless dryRun.
func (r reaper) run(snapshot string, dryRun bool) []verdict {
	var vs []verdict
	for _, p := range parseSnapshot(snapshot) {
		v, ok := r.judge(p)
		if !ok {
			continue
		}
		v.DryRun = dryRun
		if v.Kill && !dryRun {
			if err := r.signal(v.PID); err != nil {
				v.SignalErr = err.Error()
			}
		}
		logReap(v)
		vs = append(vs, v)
	}
	return vs
}

func (r reaper) judge(p proc) (verdict, bool) {
	m := claudeTempPath.FindAllStringSubmatch(p.command, -1)
	if m == nil {
		return verdict{}, false
	}
	pathUID, project, session := m[0][1], m[0][2], m[0][3]
	v := verdict{PID: p.pid, PPID: p.ppid, Age: "-", Session: session, Project: project, Idle: "-", Command: p.command}
	skip := func(reason string) (verdict, bool) {
		v.Reason = reason
		return v, true
	}
	for _, x := range m[1:] {
		if x[1] != pathUID || x[2] != project || x[3] != session {
			return skip("several session paths in command")
		}
	}
	age, ageErr := parseEtime(p.etime)
	if ageErr == nil {
		v.Age = fmtDur(age)
	}
	switch {
	case pathUID != strconv.Itoa(r.uid):
		return skip("temp path of uid " + pathUID)
	case p.uid != r.uid:
		return skip(fmt.Sprintf("owned by uid %d", p.uid))
	case p.ppid != 1:
		return skip(fmt.Sprintf("parent %d alive", p.ppid))
	case isClaude(p.command):
		return skip("claude binary")
	case session == r.self:
		return skip("own session")
	case !safeName(project):
		return skip("bad project dir")
	case ageErr != nil:
		return skip("unparsable etime " + strconv.Quote(p.etime))
	case age <= r.idle:
		return skip("process younger than " + fmtDur(r.idle))
	}
	mod, err := r.modTime(filepath.Join(r.home, ".claude", "projects", project, session+".jsonl"))
	if err != nil {
		return skip("no transcript")
	}
	idle := r.now().Sub(mod)
	v.Idle = fmtDur(idle)
	if idle <= r.idle {
		return skip("transcript active within " + fmtDur(r.idle))
	}
	v.Kill = true
	v.Reason = "orphaned, session idle"
	return v, true
}

// isClaude reports whether the executable is Claude Code itself.
func isClaude(command string) bool {
	exe, _, _ := strings.Cut(command, " ")
	return filepath.Base(exe) == "claude" ||
		strings.Contains(command, "anthropic.claude-code") ||
		strings.Contains(command, "@anthropic-ai/claude-code")
}

// parseSnapshot reads `ps -o pid=,ppid=,uid=,etime=,command=` lines,
// dropping any it cannot parse.
func parseSnapshot(s string) []proc {
	var ps []proc
	for _, line := range strings.Split(s, "\n") {
		var f [4]string
		rest := line
		for i := range f {
			f[i], rest = cutField(rest)
		}
		cmd := strings.TrimSpace(rest)
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		uid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil || cmd == "" {
			continue
		}
		ps = append(ps, proc{pid: pid, ppid: ppid, uid: uid, etime: f[3], command: cmd})
	}
	return ps
}

func cutField(s string) (field, rest string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// parseEtime parses ps etime: [[DD-]HH:]MM:SS.
func parseEtime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("bad etime %q", s)
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 || strings.Count(rest, ":") != 2 {
			return 0, bad
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, bad
	}
	limits := []int{24, 60, 60}[3-len(parts):]
	total := days
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n >= limits[i] {
			return 0, bad
		}
		total = total*limits[i] + n
	}
	return time.Duration(total) * time.Second, nil
}

func fmtDur(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	}
	return d.Truncate(time.Second).String()
}

// reapOnSessionStart never affects the hook: failures and panics are only logged.
func reapOnSessionStart(sessionID string) {
	defer func() {
		if p := recover(); p != nil {
			appendLog("reap=error", []byte(fmt.Sprint(p)))
		}
	}()
	if !safeName(sessionID) {
		// Without the hook's own session, its processes cannot be protected.
		appendLog("reap=error", []byte("no session_id"))
		return
	}
	if _, _, err := reap(sessionID, false); err != nil {
		appendLog("reap=error", []byte(err.Error()))
	}
}

// reapMain is `ctxguard reap [--dry-run]`; it returns the exit code.
func reapMain(args []string, out, errOut io.Writer) int {
	dryRun := false
	for _, a := range args {
		if a != "--dry-run" {
			fmt.Fprintln(errOut, "usage: ctxguard reap [--dry-run]")
			return 2
		}
		dryRun = true
	}
	vs, enabled, err := reap("", dryRun)
	if !enabled {
		fmt.Fprintln(out, "reaping disabled (CTXGUARD_REAP=0)")
		return 0
	}
	if err != nil {
		fmt.Fprintln(errOut, "ctxguard reap:", err)
		return 1
	}
	for _, v := range vs {
		line := fmt.Sprintf("pid=%d age=%s session=%s transcript_idle=%s %s %s", v.PID, v.Age, v.Session, v.Idle, v.label(), v.Reason)
		if v.SignalErr != "" {
			line += " (SIGTERM failed: " + v.SignalErr + ")"
		}
		fmt.Fprintln(out, line)
	}
	return 0
}

func logReap(v verdict) {
	b, _ := json.Marshal(v)
	appendLog("reap="+v.label(), b)
}
