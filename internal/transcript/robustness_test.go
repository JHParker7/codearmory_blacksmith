package transcript

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A RECORD WITH NO TIMESTAMP MUST NOT VANISH. The filename comes from it, so an
// unstamped record lands in transcripts-0001-01-01.jsonl — a file nobody will
// ever open, holding a line that is gone as far as anyone reading the corpus is
// concerned.
func TestAnUnstampedRecordIsFiledUnderToday(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewJSONLSink(dir)
	if err != nil {
		t.Fatalf("NewJSONLSink: %v", err)
	}
	t.Cleanup(func() { sink.Close() })

	if err := sink.Write(Record{TranscriptID: "tr-1", Kind: KindStart}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("wrote %d files: %v", len(files), files)
	}
	name := filepath.Base(files[0])
	if strings.Contains(name, "0001-01-01") {
		t.Fatalf("an unstamped record landed in %s, which nobody will ever look at", name)
	}
	if !strings.Contains(name, today) {
		t.Errorf("an unstamped record landed in %s, want today's file", name)
	}

	// It must still be readable, not merely filed.
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "tr-1") {
		t.Error("the record did not survive being stamped")
	}
}

// AN EMPTY DETAIL IS BETTER THAN A DECORATIVE ONE. Formatting the pair
// unconditionally and trimming leaves a lone dash — a record that says nothing,
// in the one place whose whole purpose is to say what the agent attempted.
func TestARefusalWithNothingToSayIsEmptyNotDecorated(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")

	rec.Refusal(ctx, RefusalOther, "", "")
	rec.Refusal(ctx, RefusalTestFile, "store_test.go", "")
	rec.Refusal(ctx, RefusalNoopEdit, "", "the edit would not change the file")
	rec.Refusal(ctx, RefusalStaleRead, "main.go", "read again with nothing new")

	got := sink.ofKind(KindRefusal)
	if len(got) != 4 {
		t.Fatalf("wrote %d refusals", len(got))
	}
	if got[0].Detail != "" {
		t.Errorf("an empty refusal recorded %q, which says nothing", got[0].Detail)
	}
	if got[1].Detail != "store_test.go" {
		t.Errorf("a target-only refusal recorded %q", got[1].Detail)
	}
	if got[2].Detail != "the edit would not change the file" {
		t.Errorf("a detail-only refusal recorded %q", got[2].Detail)
	}
	if got[3].Detail != "main.go — read again with nothing new" {
		t.Errorf("a full refusal recorded %q", got[3].Detail)
	}
}
