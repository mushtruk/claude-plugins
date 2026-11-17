package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A subagent's read over the cap is refused before it runs: everything it
// reads is paid again on each of its later turns. Only the read forms below
// are measured; any other call, and every error, is allowed.

const (
	defaultAgentReadCap = 8192
	readDefaultLimit    = 2000 // lines the Read tool returns without a limit
	readLinePrefix      = 7    // bytes of the line-number prefix Read and nl add to each line
	binarySniff         = 8192 // a NUL in this many leading bytes marks a binary file
	// lineBuffer holds any source line whole; a longer one is counted in
	// pieces, so a huge line never sits in memory.
	lineBuffer = 64 << 10
	// maxSkip bounds the bytes skipped before a range starts: 64 MB reads in
	// well under the hook's 5 s timeout, and a farther offset is allowed.
	maxSkip = 64 << 20

	longLineAdvice = "alone is longer than that: find what you need with `grep -n`, take only that part of the line with `cut -c`, " +
		"and read the lines around it in ranges that leave it out. "
)

// Files the Read tool renders rather than returning as text.
var readMediaExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".pdf": true, ".ipynb": true}

var sedPrint = regexp.MustCompile(`^([0-9]+)(?:,([0-9]+))?p$`)

// fileRead is the part of a file a call returns, with the bytes it adds to
// each line.
type fileRead struct {
	path   string
	lines  lineRange
	prefix int
}

type rangeKind int

const (
	rangeWhole rangeKind = iota // the whole file
	rangeFrom                   // n lines from line first
	rangeLast                   // the last n lines
)

type lineRange struct {
	kind  rangeKind
	first int // 1-based; rangeFrom only
	n     int
}

func (r lineRange) start() int {
	if r.kind == rangeFrom {
		return r.first
	}
	return 1
}

// ended reports whether line lies past the range.
func (r lineRange) ended(line int) bool {
	return r.kind == rangeFrom && line >= r.first+r.n
}

// tally adds up what a call would return, stopping just past cap.
type tally struct {
	cap, bytes, lines int
	longLine          bool
	longLineNo        int // 0 when the long line's number is unknown
}

func preToolUse(h hookInput) (any, string) {
	if h.AgentID == "" {
		return nil, "read-main"
	}
	if os.Getenv("CTXGUARD_AGENT_READ_CAP") == "0" {
		return nil, "read-off"
	}
	reads, ok := readsOf(h)
	if !ok {
		return nil, "read-unrecognised"
	}
	t := tally{cap: envInt("CTXGUARD_AGENT_READ_CAP", defaultAgentReadCap)}
	for _, r := range reads {
		if !t.count(r) {
			return nil, "read-error"
		}
		if t.over() {
			return t.deny(), fmt.Sprintf("read-capped %d", t.bytes)
		}
	}
	return nil, fmt.Sprintf("read-ok %d", t.bytes)
}

func readsOf(h hookInput) ([]fileRead, bool) {
	switch h.ToolName {
	case "Read":
		return readToolReads(h)
	case "Bash":
		return bashReads(h.ToolInput.Command, h.Cwd)
	}
	return nil, false
}

func readToolReads(h hookInput) ([]fileRead, bool) {
	in := h.ToolInput
	if isSet(in.Pages) || readMediaExt[strings.ToLower(filepath.Ext(in.FilePath))] {
		return nil, true
	}
	offset, okOffset := optInt(in.Offset, 1)
	limit, okLimit := optInt(in.Limit, readDefaultLimit)
	path, okPath := resolve(h.Cwd, in.FilePath)
	if !okOffset || !okLimit || !okPath {
		return nil, false
	}
	if limit < 1 { // measured as the default, so it never lets a whole file through unmeasured
		limit = readDefaultLimit
	}
	return []fileRead{{path: path, lines: lineRange{kind: rangeFrom, first: max(offset, 1), n: limit}, prefix: readLinePrefix}}, true
}

// bashReads recognises only `cat F...`, `nl F`, `head|tail [-n N|-N] F` and
// `sed -n 'A,Bp' F`, joined by `;` or `&&`, each optionally after `cd DIR`.
func bashReads(cmd, cwd string) ([]fileRead, bool) {
	s := strings.ReplaceAll(cmd, "&&", ";")
	if strings.ContainsAny(s, "|&<>$`*?[]{}()~!#\"\\\n\r") {
		return nil, false
	}
	dir := cwd
	var reads []fileRead
	for _, seg := range strings.Split(s, ";") {
		args, ok := unquote(strings.Fields(seg))
		if !ok {
			return nil, false
		}
		if len(args) == 0 {
			continue
		}
		if args[0] == "cd" {
			if len(args) != 2 {
				return nil, false
			}
			if dir, ok = resolve(dir, args[1]); !ok {
				return nil, false
			}
			continue
		}
		r, ok := commandReads(args, dir)
		if !ok {
			return nil, false
		}
		reads = append(reads, r...)
	}
	return reads, len(reads) > 0
}

// unquote strips single quotes that wrap a whole argument; any other quote is
// unrecognised.
func unquote(args []string) ([]string, bool) {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(a, "'") {
			inner, found := strings.CutPrefix(a, "'")
			inner, wrapped := strings.CutSuffix(inner, "'")
			if !found || !wrapped || strings.Contains(inner, "'") {
				return nil, false
			}
			a = inner
		}
		out[i] = a
	}
	return out, true
}

func commandReads(args []string, dir string) ([]fileRead, bool) {
	switch args[0] {
	case "cat":
		if len(args) < 2 {
			return nil, false
		}
		var reads []fileRead
		for _, a := range args[1:] {
			path, ok := fileArg(dir, a)
			if !ok {
				return nil, false
			}
			reads = append(reads, fileRead{path: path, lines: lineRange{kind: rangeWhole}})
		}
		return reads, true
	case "nl":
		if len(args) != 2 {
			return nil, false
		}
		path, ok := fileArg(dir, args[1])
		return []fileRead{{path: path, lines: lineRange{kind: rangeWhole}, prefix: readLinePrefix}}, ok
	case "head", "tail":
		n, file, ok := lineCountArgs(args[1:])
		if !ok {
			return nil, false
		}
		lines := lineRange{kind: rangeFrom, first: 1, n: n}
		if args[0] == "tail" {
			lines = lineRange{kind: rangeLast, n: n}
		}
		path, ok := fileArg(dir, file)
		return []fileRead{{path: path, lines: lines}}, ok
	case "sed":
		if len(args) != 4 || args[1] != "-n" {
			return nil, false
		}
		m := sedPrint.FindStringSubmatch(args[2])
		if m == nil {
			return nil, false
		}
		a, errA := strconv.Atoi(m[1])
		b := a
		var errB error
		if m[2] != "" {
			b, errB = strconv.Atoi(m[2])
		}
		path, ok := fileArg(dir, args[3])
		if errA != nil || errB != nil || a < 1 || !ok {
			return nil, false
		}
		return []fileRead{{path: path, lines: lineRange{kind: rangeFrom, first: a, n: max(b-a+1, 1)}}}, true
	}
	return nil, false
}

// lineCountArgs parses `[-n N | -N] F`; head and tail print 10 lines by default.
func lineCountArgs(args []string) (int, string, bool) {
	switch {
	case len(args) == 1:
		return 10, args[0], true
	case len(args) == 2 && strings.HasPrefix(args[0], "-"):
		n, ok := digits(args[0][1:])
		return n, args[1], ok
	case len(args) == 3 && args[0] == "-n":
		n, ok := digits(args[1])
		return n, args[2], ok
	}
	return 0, "", false
}

func digits(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func fileArg(dir, a string) (string, bool) {
	if strings.HasPrefix(a, "-") {
		return "", false
	}
	return resolve(dir, a)
}

func resolve(dir, p string) (string, bool) {
	switch {
	case p == "":
		return "", false
	case filepath.IsAbs(p):
		return p, true
	case dir == "":
		return "", false
	}
	return filepath.Join(dir, p), true
}

func isSet(raw json.RawMessage) bool {
	s := string(raw)
	return s != "" && s != "null" && s != `""`
}

func optInt(raw json.RawMessage, def int) (int, bool) {
	if !isSet(raw) {
		return def, true
	}
	var n int
	return n, json.Unmarshal(raw, &n) == nil
}

// add counts a line of n bytes; lineNo is 0 when its number is unknown.
func (t *tally) add(n, lineNo int) {
	t.bytes += n
	t.lines++
	if n > t.cap {
		t.longLine, t.longLineNo = true, lineNo
	}
}

func (t *tally) over() bool { return t.bytes > t.cap }

// count adds what r would return; false on any error. A binary file adds
// nothing, and nothing but a regular file is opened.
func (t *tally) count(r fileRead) bool {
	info, err := os.Stat(r.path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(r.path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, binarySniff)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return true
	}
	if r.lines.kind == rangeLast {
		return t.countTail(f, info.Size(), r)
	}
	return t.countLines(io.MultiReader(bytes.NewReader(head[:n]), f), r)
}

// countLines stops as soon as the tally is over the cap, and gives up after
// maxSkip bytes before the first line, so a huge file costs one bounded read.
func (t *tally) countLines(rd io.Reader, r fileRead) bool {
	br := bufio.NewReaderSize(rd, lineBuffer)
	line, length, read := 1, 0, 0
	for !r.lines.ended(line) {
		chunk, err := br.ReadSlice('\n')
		length += len(chunk)
		read += len(chunk)
		inRange := line >= r.lines.start()
		switch {
		case err == bufio.ErrBufferFull:
			if inRange && t.bytes+length+r.prefix > t.cap {
				t.add(length+r.prefix, line)
				return true
			}
			if !inRange && read > maxSkip {
				return false
			}
			continue
		case err == io.EOF:
			if inRange && length > 0 {
				t.add(length+r.prefix, line)
			}
			return true
		case err != nil:
			return false
		}
		if inRange {
			t.add(length+r.prefix, line)
			if t.over() {
				return true
			}
		} else if read > maxSkip {
			return false
		}
		line++
		length = 0
	}
	return true
}

// countTail reads at most cap+1 bytes from the end: a tail longer than that
// is over the cap whatever precedes it.
func (t *tally) countTail(f *os.File, size int64, r fileRead) bool {
	if r.lines.n == 0 {
		return true
	}
	l := min(size, int64(t.cap)+1)
	buf := make([]byte, l)
	if _, err := f.ReadAt(buf, size-l); err != nil {
		return false
	}
	body := bytes.TrimSuffix(buf, []byte("\n"))
	start, newlines := 0, 0
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '\n' {
			if newlines++; newlines == r.lines.n {
				start = i + 1
				break
			}
		}
	}
	for b := buf[start:]; len(b) > 0; {
		n := len(b)
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			n = i + 1
		}
		t.add(n+r.prefix, 0)
		b = b[n:]
	}
	return true
}

func (t *tally) deny() map[string]any {
	reason := fmt.Sprintf("READ CAP: this call would return more than the %d KB a subagent may read at once (an estimate). ", (t.cap+1023)/1024)
	switch {
	case t.longLineNo > 0:
		reason += fmt.Sprintf("Line %d ", t.longLineNo) + longLineAdvice
	case t.longLine:
		reason += "One line in this range " + longLineAdvice
	default:
		reason += fmt.Sprintf("Find the place with `grep -n`, then read at most %d lines of it with Read offset/limit or `sed -n 'A,Bp'`. ", max(t.cap*t.lines/t.bytes, 1))
	}
	reason += "Do not re-read content your brief already inlines."
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}}
}
