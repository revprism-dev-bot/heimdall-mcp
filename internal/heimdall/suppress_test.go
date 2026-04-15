package heimdall

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestShouldEmitTierB_FirstCallTrue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suppress.db")
	got, err := ShouldEmitTierBWithPath(path, "/proj/a", "ollama_down", 5*time.Minute)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !got {
		t.Error("first sighting should emit")
	}
}

func TestShouldEmitTierB_SecondCallWithinWindowFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suppress.db")

	if got, _ := ShouldEmitTierBWithPath(path, "/proj/a", "ollama_down", 5*time.Minute); !got {
		t.Fatal("first call should emit")
	}
	got, err := ShouldEmitTierBWithPath(path, "/proj/a", "ollama_down", 5*time.Minute)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got {
		t.Error("second call within window should suppress")
	}
}

// Use shouldEmit directly with an injected clock so we can cross the window
// boundary without sleeping.
func TestShouldEmitTierB_AfterWindowTrue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suppress.db")
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE suppression (key TEXT PRIMARY KEY, last_emitted INTEGER NOT NULL);`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	t0 := time.Unix(1_700_000_000, 0)
	if !shouldEmit(db, "/proj/a", "code", time.Minute, t0) {
		t.Fatal("first call should emit")
	}
	// 30s later → suppressed
	if shouldEmit(db, "/proj/a", "code", time.Minute, t0.Add(30*time.Second)) {
		t.Error("within-window call should suppress")
	}
	// 61s later → re-emit
	if !shouldEmit(db, "/proj/a", "code", time.Minute, t0.Add(61*time.Second)) {
		t.Error("post-window call should emit")
	}
}

func TestShouldEmitTierB_IndependentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suppress.db")

	if got, _ := ShouldEmitTierBWithPath(path, "/proj/a", "code1", time.Minute); !got {
		t.Fatal("proj-a code1: want emit")
	}
	// Different project, same code — independent.
	if got, _ := ShouldEmitTierBWithPath(path, "/proj/b", "code1", time.Minute); !got {
		t.Error("proj-b code1: want emit")
	}
	// Same project, different code — independent.
	if got, _ := ShouldEmitTierBWithPath(path, "/proj/a", "code2", time.Minute); !got {
		t.Error("proj-a code2: want emit")
	}
	// Re-check: proj-a code1 should still be suppressed.
	if got, _ := ShouldEmitTierBWithPath(path, "/proj/a", "code1", time.Minute); got {
		t.Error("proj-a code1 repeat: want suppress")
	}
}

func TestShouldEmitTierB_CrossRestartPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suppress.db")

	// "Session 1": open, emit, close.
	if got, err := ShouldEmitTierBWithPath(path, "/proj/a", "model_mismatch", time.Hour); err != nil || !got {
		t.Fatalf("session 1: %v / %v", err, got)
	}

	// "Session 2": reopen — suppression should still apply.
	if got, err := ShouldEmitTierBWithPath(path, "/proj/a", "model_mismatch", time.Hour); err != nil {
		t.Fatalf("session 2 err: %v", err)
	} else if got {
		t.Error("suppression did not persist across reopen")
	}
}

// suppressKey must be stable for the same inputs (otherwise persistence
// is a lie) and sensitive to each of the two fields.
func TestSuppressKey_StableAndSensitive(t *testing.T) {
	a := suppressKey("/proj", "code")
	b := suppressKey("/proj", "code")
	if a != b {
		t.Error("key not stable")
	}
	if suppressKey("/proj", "code") == suppressKey("/other", "code") {
		t.Error("project change did not change key")
	}
	if suppressKey("/proj", "code") == suppressKey("/proj", "other") {
		t.Error("code change did not change key")
	}
}
