// Package audit is an append-only log of every mutating API call,
// including calls that were denied or run as dry runs.
package audit

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Entry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	DryRun bool      `json:"dry_run"`
	Result string    `json:"result"`
}

type Log struct {
	mu      sync.Mutex
	entries []Entry
	file    *os.File
	enc     *json.Encoder
}

// Open returns a Log. If path is empty the log stays in memory only.
// If path is set, entries are also appended to that file as JSONL.
func Open(path string) (*Log, error) {
	l := &Log{}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		l.file = f
		l.enc = json.NewEncoder(f)
	}
	return l, nil
}

func (l *Log) Record(actor, action, target string, dryRun bool, result string) {
	e := Entry{
		Time:   time.Now().UTC(),
		Actor:  actor,
		Action: action,
		Target: target,
		DryRun: dryRun,
		Result: result,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if l.enc != nil {
		_ = l.enc.Encode(e)
	}
}

func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

func (l *Log) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
