package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sink is where records go.
//
// AN INTERFACE BECAUSE THE LOCAL FILE IS THE STARTING POINT, NOT THE
// DESTINATION: transcripts eventually want to reach an artifact store, so a
// training corpus outlives one workstation's disk.
type Sink interface {
	Write(Record) error
	Close() error
}

// JSONLSink appends records to a newline-delimited JSON file, rotating daily.
//
// EVERY WRITE GOES STRAIGHT TO THE OS. The process being killed is a normal
// event on a workstation, and a buffered final record is exactly the one worth
// keeping — the last thing an agent did before it stopped is what explains why
// it stopped.
type JSONLSink struct {
	dir string

	mu   sync.Mutex
	file *os.File
	day  string
}

// NewJSONLSink prepares a sink writing into dir, creating it if needed.
func NewJSONLSink(dir string) (*JSONLSink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("transcript dir %s: %w", dir, err)
	}
	return &JSONLSink{dir: dir}, nil
}

func (s *JSONLSink) Write(r Record) error {
	// A RECORD WITH NO TIMESTAMP IS STAMPED HERE, because the filename is derived
	// from it. An unstamped record otherwise lands in transcripts-0001-01-01.jsonl
	// -- a file nobody will ever open, holding a line that is gone as far as anyone
	// reading the corpus is concerned.
	//
	// The recorder already stamps, so this is the second line of defence rather
	// than the first. It is here because the sink is exported and writable
	// directly, and silently losing a record is the one failure this package must
	// not have.
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}

	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode transcript record: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateLocked(r.At); err != nil {
		return err
	}
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append transcript record: %w", err)
	}
	return nil
}

// rotateLocked opens the file for the record's day, closing yesterday's.
//
// Daily files keep any single file readable and make "what did the department do
// on this date" answerable with a filename.
func (s *JSONLSink) rotateLocked(at time.Time) error {
	day := at.UTC().Format("2006-01-02")
	if s.file != nil && s.day == day {
		return nil
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	path := filepath.Join(s.dir, "transcripts-"+day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open transcript file %s: %w", path, err)
	}
	s.file, s.day = f, day
	return nil
}

func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
