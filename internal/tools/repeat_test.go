package tools

import (
	"context"
	"strings"
	"testing"
)

// THE LOOP THAT KILLED THE THIRD TWO-STAGE ATTEMPT. An architect called
// list_files fifteen times against an empty repository, one second apart, wrote
// nothing, and nothing anywhere told it the answer had not changed. read_files
// was the only tool that said so, so the loop simply moved to a tool that did
// not.
func TestARepeatedListIsEventuallyCalledOut(t *testing.T) {
	s := newSet(map[string]string{}, AllowAll, nil)
	ctx := context.Background()

	var last string
	for i := 0; i < MaxRepeatedCalls+1; i++ {
		got, err := s.Invoke(ctx, ListFiles, `{}`)
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		last = got
	}
	if !strings.Contains(last, "times in a row") {
		t.Fatalf("a repeated list was never called out:\n%s", last)
	}
	if !strings.Contains(last, WriteFile) {
		t.Fatalf("the notice does not say what would move things on:\n%s", last)
	}
}

// The first few identical calls are not a loop. A stage that lists twice while
// deciding what to do must not be nagged for it.
func TestTheFirstRepeatsAreLeftAlone(t *testing.T) {
	s := newSet(map[string]string{}, AllowAll, nil)

	got, err := s.Invoke(context.Background(), ListFiles, `{}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(got, "times in a row") {
		t.Fatalf("the very first call was called out:\n%s", got)
	}
	got, _ = s.Invoke(context.Background(), ListFiles, `{}`)
	if strings.Contains(got, "times in a row") {
		t.Fatalf("the second call was called out:\n%s", got)
	}
}

// A call that returns something DIFFERENT resets the count — that is the agent
// making progress, and the whole point is to catch the case where it is not.
func TestADifferentAnswerResetsTheCount(t *testing.T) {
	s := newSet(map[string]string{"a.go": "package p\n"}, AllowAll, nil)
	ctx := context.Background()

	for i := 0; i < MaxRepeatedCalls+1; i++ {
		if _, err := s.Invoke(ctx, ListFiles, `{}`); err != nil {
			t.Fatalf("invoke: %v", err)
		}
	}
	// A write changes what list_files returns.
	if _, err := s.Invoke(ctx, WriteFile,
		`{"path":"b.go","replace":"package p\n","summary":"x","type":"feat"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.Invoke(ctx, ListFiles, `{}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(got, "times in a row") {
		t.Fatalf("the count was not reset by a changed answer:\n%s", got)
	}
}

// read_files explains itself in content-aware terms already; a second, vaguer
// notice saying the same thing is noise.
func TestReadFilesIsNotDoubleNagged(t *testing.T) {
	s := newSet(map[string]string{"a.go": "package p\n"}, AllowAll, nil)
	ctx := context.Background()

	var last string
	for i := 0; i < MaxRepeatedCalls+2; i++ {
		got, err := s.Invoke(ctx, ReadFiles, `{"paths":["a.go"]}`)
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		last = got
	}
	if !strings.Contains(last, "NOTE: you already had") {
		t.Fatalf("the content-aware notice is missing:\n%s", last)
	}
	if strings.Contains(last, "times in a row that read_files") {
		t.Fatalf("read_files was nagged twice for the same thing:\n%s", last)
	}
}

// The repeated answer itself is still served. Withholding it would break the
// edit format, which requires quoting text exactly as it stands.
func TestTheRepeatedAnswerIsStillServed(t *testing.T) {
	s := newSet(map[string]string{"a.go": "package p\n"}, AllowAll, nil)
	ctx := context.Background()

	var last string
	for i := 0; i < MaxRepeatedCalls+1; i++ {
		last, _ = s.Invoke(ctx, ListFiles, `{}`)
	}
	if !strings.Contains(last, "a.go") {
		t.Fatalf("the answer was withheld:\n%s", last)
	}
}
