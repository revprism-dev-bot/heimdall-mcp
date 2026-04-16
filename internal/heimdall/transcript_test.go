package heimdall

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestTranscriptPathForSession_BuildsSlug(t *testing.T) {
	got, err := TranscriptPathForSession("/home/alice/Code/proj", "abc-123", "/home/alice")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := filepath.Join("/home/alice", ".claude", "projects", "-home-alice-Code-proj", "abc-123.jsonl")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTranscriptPathForSession_EmptySessionErrors(t *testing.T) {
	_, err := TranscriptPathForSession("/home/alice/Code/proj", "", "/home/alice")
	if err == nil {
		t.Fatalf("expected error on empty session id")
	}
}

func TestTranscriptPathForSession_WindowsPathIgnoredOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only slug rule")
	}
	got, _ := TranscriptPathForSession("/home/alice/Code/proj", "s", "/home/alice")
	if filepath.Base(filepath.Dir(got)) != "-home-alice-Code-proj" {
		t.Fatalf("unexpected slug dir: %s", got)
	}
}
