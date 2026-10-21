package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// emitter appends Interaction records to a per-session JSONL file. It holds a
// buffered writer per session, flushed on every emit so an external consumer
// tailing the file sees records immediately.
type emitter struct {
	dir string
	mu  sync.Mutex
	// files caches open handles keyed by session ID.
	files   map[string]*bufio.Writer
	handles map[string]*os.File
}

func newEmitter(dir string) (*emitter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create interactions dir: %w", err)
	}
	return &emitter{dir: dir, files: map[string]*bufio.Writer{}, handles: map[string]*os.File{}}, nil
}

// emit writes one Interaction as a single JSON line to the session's file and
// flushes it so tailers observe it immediately.
func (e *emitter) emit(it Interaction) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.files[it.SessionID]
	if !ok {
		path := filepath.Join(e.dir, it.SessionID+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open interaction log: %w", err)
		}
		w = bufio.NewWriterSize(f, 8*1024)
		e.files[it.SessionID] = w
		e.handles[it.SessionID] = f
	}
	line, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("marshal interaction: %w", err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write interaction: %w", err)
	}
	return w.Flush()
}

// close flushes and closes all open handles.
func (e *emitter) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, w := range e.files {
		_ = w.Flush()
		if f, ok := e.handles[id]; ok {
			_ = f.Close()
		}
	}
	e.files = map[string]*bufio.Writer{}
	e.handles = map[string]*os.File{}
}
