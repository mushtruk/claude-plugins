package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sessA   = "586d9731-293a-4b4e-85cb-1840f3533253"
	sessB   = "6141fe1e-e04e-4a68-a000-000000000001"
	project = "-Users-x-proj"
)

var reapNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func psLine(pid, ppid, uid int, etime, cmd string) string {
	return fmt.Sprintf("%5d %5d %5d %11s %s", pid, ppid, uid, etime, cmd)
}

func scratch(uid int, session string) string {
	return fmt.Sprintf("/private/tmp/claude-%d/%s/%s/scratchpad/", uid, project, session)
}

func transcript(session string) string {
	return filepath.Join("/h", ".claude", "projects", project, session+".jsonl")
}

// testReaper: uid 501, idle 24h, sessA's transcript idle 15 days, sessB's 1 hour.
func testReaper(signalled *[]int) reaper {
	mods := map[string]time.Time{
		transcript(sessA): reapNow.Add(-15 * 24 * time.Hour),
		transcript(sessB): reapNow.Add(-time.Hour),
	}
	return reaper{
		uid:  501,
		home: "/h",
		self: "11111111-2222-3333-4444-555555555555",
		idle: 24 * time.Hour,
		now:  func() time.Time { return reapNow },
		modTime: func(p string) (time.Time, error) {
			if m, ok := mods[p]; ok {
				return m, nil
			}
			return time.Time{}, fs.ErrNotExist
		},
		signal: func(pid int) error {
			*signalled = append(*signalled, pid)
			return nil
		},
	}
}

func TestReapJudge(t *testing.T) {
	setup(t)
	serve := "node " + scratch(501, sessA) + "serve.mjs 3002"
	cases := []struct {
		name   string
		line   string
		self   string
		kill   bool
		reason string
	}{
		{"happy kill", psLine(52721, 1, 501, "16-00:36:18", serve), "", true, "orphaned"},
		{"tmp without private", psLine(7, 1, 501, "16-00:36:18", "python3 /tmp/claude-501/"+project+"/"+sessA+"/x.py"), "", true, "orphaned"},
		{"owned by another user", psLine(7, 1, 502, "16-00:36:18", serve), "", false, "owned by uid 502"},
		{"parent alive", psLine(7, 4242, 501, "16-00:36:18", serve), "", false, "parent 4242"},
		{"another uid's temp path", psLine(7, 1, 501, "16-00:36:18", "node "+scratch(502, sessA)+"serve.mjs"), "", false, "temp path of uid 502"},
		{"own session", psLine(7, 1, 501, "16-00:36:18", serve), sessA, false, "own session"},
		{"missing transcript", psLine(7, 1, 501, "16-00:36:18", "node "+scratch(501, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")+"s.mjs"), "", false, "no transcript"},
		{"transcript active", psLine(7, 1, 501, "16-00:36:18", "node "+scratch(501, sessB)+"s.mjs"), "", false, "transcript active"},
		{"process too young", psLine(7, 1, 501, "23:59:59", serve), "", false, "younger than"},
		{"unparsable etime", psLine(7, 1, 501, "garbage", serve), "", false, "etime"},
		{"claude binary", psLine(7, 1, 501, "16-00:36:18", "/Users/x/.local/bin/claude --add-dir "+scratch(501, sessA)), "", false, "claude"},
		{"claude extension binary", psLine(7, 1, 501, "16-00:36:18", "/Users/x/.vscode/extensions/anthropic.claude-code-2.1.3-darwin-arm64/resources/native-binary/claude-x "+scratch(501, sessA)), "", false, "claude"},
		{"two sessions in one command", psLine(7, 1, 501, "16-00:36:18", serve+" "+scratch(501, sessB)), "", false, "several"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sig []int
			r := testReaper(&sig)
			if tc.self != "" {
				r.self = tc.self
			}
			vs := r.run(tc.line, false)
			if len(vs) != 1 {
				t.Fatalf("want 1 candidate, got %+v", vs)
			}
			v := vs[0]
			if v.Kill != tc.kill || !strings.Contains(v.Reason, tc.reason) {
				t.Fatalf("got kill=%v reason=%q, want kill=%v reason~%q", v.Kill, v.Reason, tc.kill, tc.reason)
			}
			if tc.kill != (len(sig) == 1) {
				t.Fatalf("signalled %v, kill=%v", sig, tc.kill)
			}
		})
	}
}

func TestReapIgnoresProcessesWithoutTempPath(t *testing.T) {
	setup(t)
	var sig []int
	snap := strings.Join([]string{
		psLine(10, 1, 501, "30-00:00:00", "com.docker.backend"),
		psLine(11, 1, 501, "30-00:00:00", "kind-control-plane"),
		psLine(12, 1, 501, "30-00:00:00", "node /Users/x/projects/app/server.js"),
		psLine(13, 1, 501, "30-00:00:00", "node /private/tmp/claude-501/not-a-session/serve.mjs"),
		"not a ps line",
	}, "\n")
	if vs := testReaper(&sig).run(snap, false); len(vs) != 0 || len(sig) != 0 {
		t.Fatalf("got %+v, signalled %v", vs, sig)
	}
}

func TestReapDryRunSignalsNothing(t *testing.T) {
	setup(t)
	var sig []int
	snap := psLine(52721, 1, 501, "16-00:36:18", "node "+scratch(501, sessA)+"serve.mjs") + "\n" +
		psLine(9, 1, 501, "16-00:36:18", "node "+scratch(501, sessB)+"serve.mjs") + "\n"
	vs := testReaper(&sig).run(snap, true)
	if len(vs) != 2 || !vs[0].Kill || vs[1].Kill || !vs[0].DryRun {
		t.Fatalf("got %+v", vs)
	}
	if len(sig) != 0 {
		t.Fatalf("dry run signalled %v", sig)
	}
	sig = nil
	testReaper(&sig).run(snap, false)
	if len(sig) != 1 || sig[0] != 52721 {
		t.Fatalf("signalled %v, want [52721]", sig)
	}
}

func TestReapLogsVerdicts(t *testing.T) {
	setup(t)
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("CTXGUARD_LOG", log)
	var sig []int
	snap := psLine(52721, 1, 501, "16-00:36:18", "node "+scratch(501, sessA)+"serve.mjs") + "\n" +
		psLine(9, 4242, 501, "16-00:36:18", "node "+scratch(501, sessA)+"serve.mjs")
	testReaper(&sig).run(snap, false)
	b, _ := os.ReadFile(log)
	s := string(b)
	if !strings.Contains(s, "reap=KILL") || !strings.Contains(s, `"pid":52721`) ||
		!strings.Contains(s, "reap=SKIP") || !strings.Contains(s, `"pid":9`) {
		t.Fatalf("log %q", s)
	}
}

func TestParseEtime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"16-00:36:18", 16*24*time.Hour + 36*time.Minute + 18*time.Second, true},
		{"01-10:31:02", 34*time.Hour + 31*time.Minute + 2*time.Second, true},
		{"20:27:53", 20*time.Hour + 27*time.Minute + 53*time.Second, true},
		{"05:07", 5*time.Minute + 7*time.Second, true},
		{"  00:01", time.Second, true},
		{"", 0, false},
		{"1:2:3:4", 0, false},
		{"x:00", 0, false},
		{"1-05:07", 0, false},
		{"00:60", 0, false},
	}
	for _, tc := range cases {
		got, err := parseEtime(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseEtime(%q) = %v, %v; want %v ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestReapConfig(t *testing.T) {
	t.Setenv("CTXGUARD_REAP", "")
	t.Setenv("CTXGUARD_REAP_IDLE", "")
	r, on, err := newReaper("s")
	if err != nil || !on || r.idle != defaultReapIdle || r.self != "s" {
		t.Fatalf("default: %+v on=%v err=%v", r, on, err)
	}
	t.Setenv("CTXGUARD_REAP_IDLE", "36h")
	if r, _, _ := newReaper("s"); r.idle != 36*time.Hour {
		t.Fatalf("idle %v", r.idle)
	}
	t.Setenv("CTXGUARD_REAP_IDLE", "bogus")
	if r, _, _ := newReaper("s"); r.idle != defaultReapIdle {
		t.Fatalf("bogus idle %v", r.idle)
	}
	t.Setenv("CTXGUARD_REAP", "0")
	if _, on, _ := newReaper("s"); on {
		t.Fatal("CTXGUARD_REAP=0 should disable")
	}
}

func TestReapCommandDisabled(t *testing.T) {
	setup(t)
	t.Setenv("CTXGUARD_REAP", "0")
	psSnapshot = func() (string, error) { t.Fatal("ps called while disabled"); return "", nil }
	var out, errOut bytes.Buffer
	if code := reapMain([]string{"--dry-run"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "disabled") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errOut.String())
	}
	if code := reapMain([]string{"--bogus"}, &out, &errOut); code == 0 {
		t.Fatal("unknown flag accepted")
	}
}

// hookHome makes a HOME whose transcripts for sessA and sessB are 15 days idle.
func hookHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := time.Now().Add(-15 * 24 * time.Hour)
	for _, s := range []string{sessA, sessB} {
		p := writeLines(t, filepath.Join(home, ".claude", "projects", project, s+".jsonl"), "{}")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHookReapsOnSessionStart(t *testing.T) {
	uid := os.Getuid()
	snap := psLine(100, 1, uid, "16-00:00:00", "node "+scratch(uid, sessA)+"serve.mjs") + "\n" +
		psLine(200, 1, uid, "16-00:00:00", "node "+scratch(uid, sessB)+"serve.mjs") + "\n"
	for _, source := range []string{"startup", "resume", "compact", "clear"} {
		t.Run(source, func(t *testing.T) {
			setup(t)
			hookHome(t)
			var sig []int
			psSnapshot = func() (string, error) { return snap, nil }
			signalTerm = func(pid int) error { sig = append(sig, pid); return nil }
			runHook(t, map[string]any{"hook_event_name": "SessionStart", "source": source, "session_id": sessB, "cwd": t.TempDir()})
			if len(sig) != 1 || sig[0] != 100 {
				t.Fatalf("signalled %v, want [100] (never the hook's own session)", sig)
			}
		})
	}
	t.Run("other events do not reap", func(t *testing.T) {
		setup(t)
		psSnapshot = func() (string, error) { t.Fatal("ps called"); return "", nil }
		runHook(t, map[string]any{"hook_event_name": "Stop", "session_id": sessB, "transcript_path": "/nope"})
	})
	t.Run("disabled", func(t *testing.T) {
		setup(t)
		t.Setenv("CTXGUARD_REAP", "0")
		psSnapshot = func() (string, error) { t.Fatal("ps called while disabled"); return "", nil }
		runHook(t, map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": sessB})
	})
	t.Run("no session id", func(t *testing.T) {
		setup(t)
		psSnapshot = func() (string, error) { t.Fatal("ps called without a session to protect"); return "", nil }
		runHook(t, map[string]any{"hook_event_name": "SessionStart", "source": "startup"})
	})
}

func TestHookReapErrorKeepsOutput(t *testing.T) {
	for name, ps := range map[string]func() (string, error){
		"error": func() (string, error) { return "", errors.New("ps exploded") },
		"panic": func() (string, error) { panic("ps panicked") },
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			log := filepath.Join(t.TempDir(), "log")
			t.Setenv("CTXGUARD_LOG", log)
			cwd := t.TempDir()
			writeLines(t, filepath.Join(cwd, ".claude", "handoffs", "main-1.md"), "state")
			ev := map[string]any{"hook_event_name": "SessionStart", "source": "compact", "session_id": sessB, "cwd": cwd}

			t.Setenv("CTXGUARD_REAP", "0")
			want := runHook(t, ev)
			t.Setenv("CTXGUARD_REAP", "")
			psSnapshot = ps
			got := runHook(t, ev)
			if additional(got) == "" || additional(got) != additional(want) {
				t.Fatalf("output changed by reaper failure: %v vs %v", got, want)
			}
			if b, _ := os.ReadFile(log); !strings.Contains(string(b), "reap=error") {
				t.Fatalf("reap error not logged: %q", b)
			}
		})
	}
}
