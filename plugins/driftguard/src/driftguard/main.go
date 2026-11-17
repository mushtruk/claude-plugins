// driftguard tells Claude Code sessions working in parallel git worktrees
// which files their branch shares with the default branch and with sibling
// branches, early enough to merge while the overlap is small.
//
//	driftguard hook   SessionStart / UserPromptSubmit hook (stdin: hook JSON)
//	driftguard post --summary S --action A [--paths p1,p2]
//	                  announce a contract change to other branches' sessions
//	driftguard claim --summary S --paths p1,p2
//	                  own a shared surface until this branch lands
//	driftguard release
//	                  end every claim this branch posted
//
// The hook never fails loudly: any error exits 0 without output and leaves a
// line in <state dir>/log.jsonl. The state dir is $DRIFTGUARD_STATE_DIR, else
// ~/.claude/state/driftguard. The other subcommands report errors on stderr
// and exit 1, or 2 on a usage error.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	// logMax caps log.jsonl before it rotates to log.jsonl.1: about 4000
	// runs, days of history, while the log never holds more than twice this
	// on disk.
	logMax = 1 << 20
	// gitTimeout is 25 times the slowest local git call measured on a repo
	// with 40 worktrees, room for a cold cache.
	gitTimeout = 2 * time.Second
	// fetchTimeout covers one ssh round trip (about 1.6 s) and the one or
	// two more a small fetch of the default branch needs.
	fetchTimeout = 4 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "hook":
		hookMain()
	default:
		os.Exit(cliMain(os.Args[1:]))
	}
}

func cliMain(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(os.Stderr, err)
	}
	p := poster{git: gitRunner{timeout: gitTimeout, fetchTimeout: fetchTimeout}, now: time.Now}
	return p.run(cwd, args, os.Stdout, os.Stderr)
}

func hookMain() {
	dir, ok := stateDir()
	if !ok {
		return
	}
	logger, closer, err := openLog(dir)
	if err != nil {
		logger = slog.New(slog.DiscardHandler)
	} else {
		// Every log line is already written through; a close error loses nothing.
		defer func() { _ = closer.Close() }()
	}
	w := watcher{
		git:    gitRunner{timeout: gitTimeout, fetchTimeout: fetchTimeout},
		now:    time.Now,
		states: states{dir: dir},
		log:    logger,
	}
	w.hook(os.Stdin, os.Stdout)
}

// stateDir is $DRIFTGUARD_STATE_DIR, else ~/.claude/state/driftguard; false
// when neither is known.
func stateDir() (string, bool) {
	if d := os.Getenv("DRIFTGUARD_STATE_DIR"); d != "" {
		return d, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".claude", "state", "driftguard"), true
}

func writeJSON(out io.Writer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := out.Write(b.Bytes())
	return err
}

// openLog opens <dir>/log.jsonl for structured JSON lines, first rotating it
// to log.jsonl.1 when it is over logMax. Two runs rotating at once may lose a
// few lines, which a debug log can afford.
func openLog(dir string) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "log.jsonl")
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMax {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewJSONHandler(f, nil)), f, nil
}
