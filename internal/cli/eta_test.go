package cli

import (
	"strings"
	"testing"
	"time"
)

func TestComputeETA_Normal(t *testing.T) {
	// 50 chunks in 10s across 50 files, 50 remaining => ~10s ETA
	got := computeETA(50, 50, 100, 10*time.Second)
	if got == "" {
		t.Fatal("expected non-empty ETA")
	}
	if !strings.Contains(got, "ETA:") {
		t.Fatalf("expected ETA label, got %q", got)
	}
	if !strings.Contains(got, "10s") {
		t.Fatalf("expected ~10s ETA, got %q", got)
	}
}

func TestComputeETA_ZeroChunks(t *testing.T) {
	got := computeETA(0, 50, 100, 10*time.Second)
	if got != "" {
		t.Fatalf("expected empty for zero chunks, got %q", got)
	}
}

func TestComputeETA_ZeroElapsed(t *testing.T) {
	got := computeETA(50, 50, 100, 0)
	if got != "" {
		t.Fatalf("expected empty for zero elapsed, got %q", got)
	}
}

func TestComputeETA_AllFilesProcessed(t *testing.T) {
	got := computeETA(100, 100, 100, 10*time.Second)
	if got != "" {
		t.Fatalf("expected empty when all files done, got %q", got)
	}
}

func TestComputeETA_NegativeValues(t *testing.T) {
	got := computeETA(-1, 50, 100, 10*time.Second)
	if got != "" {
		t.Fatalf("expected empty for negative chunks, got %q", got)
	}
}

func TestComputeETA_SingleFile(t *testing.T) {
	// 10 chunks in 5s for 1 file, 9 remaining => 10 avg chunks/file * 9 = 90 chunks / 2 per sec = 45s
	got := computeETA(10, 1, 10, 5*time.Second)
	if got == "" {
		t.Fatal("expected non-empty ETA for single file processed")
	}
	if !strings.Contains(got, "ETA:") {
		t.Fatalf("expected ETA label, got %q", got)
	}
}
