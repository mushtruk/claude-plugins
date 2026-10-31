package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
)

const (
	smallTail = 512 << 10
	largeTail = 4 << 20
)

// usageTokens sums the three input-side counters of a usage object.
func usageTokens(u map[string]any) int {
	total := 0
	for _, k := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		switch v := u[k].(type) {
		case float64:
			total += int(v)
		case json.Number:
			n, _ := v.Int64()
			total += int(n)
		}
	}
	return total
}

type record struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		Usage map[string]any `json:"usage"`
	} `json:"message"`
}

// transcriptContext returns the context size recorded by the last assistant
// record carrying usage. With skipSidechain, sidechain records are ignored
// (main transcript). Only the tail of the file is read: 512 KB, then 4 MB.
func transcriptContext(path string, skipSidechain bool) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, false
	}
	size := st.Size()
	for _, tail := range []int64{smallTail, largeTail} {
		n, ok := scanTail(f, size, tail, skipSidechain)
		if ok {
			return n, true
		}
		if size <= tail {
			break
		}
	}
	return 0, false
}

func scanTail(f io.ReaderAt, size, tail int64, skipSidechain bool) (int, bool) {
	off := size - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return 0, false
	}
	if off > 0 {
		// Drop the partial first line.
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return 0, false
		}
		buf = buf[i+1:]
	}
	usageKey := []byte(`"usage"`)
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		line := buf[start:end]
		end = start - 1
		if !bytes.Contains(line, usageKey) {
			continue
		}
		var r record
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		if r.Type != "assistant" || (skipSidechain && r.IsSidechain) || r.Message == nil || len(r.Message.Usage) == 0 {
			continue
		}
		return usageTokens(r.Message.Usage), true
	}
	return 0, false
}

var (
	launchKey   = []byte(`"async_launched"`)
	notifyKey   = []byte(`<task-notification>`)
	queueOpKey  = []byte(`"type":"queue-operation"`)
	taskIDOpen  = []byte(`<task-id>`)
	taskIDClose = []byte(`</task-id>`)
)

type runningAgent struct {
	ID, Description string
}

func (a runningAgent) String() string {
	if a.Description == "" {
		return a.ID
	}
	return a.ID + " (" + a.Description + ")"
}

type launchRecord struct {
	Type          string `json:"type"`
	IsSidechain   bool   `json:"isSidechain"`
	ToolUseResult *struct {
		Status      string `json:"status"`
		AgentID     string `json:"agentId"`
		Description string `json:"description"`
	} `json:"toolUseResult"`
}

// runningAgents returns the background agents the main thread launched and
// has not yet received a task-notification for, in launch order. The whole
// file is read: a launch can be far behind the tail.
func runningAgents(path string) ([]runningAgent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	running := map[string]string{}
	var order []string
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		switch {
		case bytes.Contains(line, launchKey):
			var rec launchRecord
			if json.Unmarshal(line, &rec) == nil && rec.Type == "user" && !rec.IsSidechain &&
				rec.ToolUseResult != nil && rec.ToolUseResult.Status == "async_launched" && rec.ToolUseResult.AgentID != "" {
				id := rec.ToolUseResult.AgentID
				if _, ok := running[id]; !ok {
					order = append(order, id)
				}
				running[id] = rec.ToolUseResult.Description
			}
		case bytes.Contains(line, notifyKey) && !bytes.Contains(line, queueOpKey):
			// Only the delivered notification counts, not the queue entry.
			for rest := line; ; {
				i := bytes.Index(rest, taskIDOpen)
				if i < 0 {
					break
				}
				rest = rest[i+len(taskIDOpen):]
				j := bytes.Index(rest, taskIDClose)
				if j < 0 {
					break
				}
				delete(running, string(rest[:j]))
				rest = rest[j:]
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	var agents []runningAgent
	for _, id := range order {
		if desc, ok := running[id]; ok {
			agents = append(agents, runningAgent{id, desc})
		}
	}
	return agents, nil
}
