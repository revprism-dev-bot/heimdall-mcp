package heimdall

import (
	"strings"
	"testing"
	"time"
)

// helper: insert a record with specific last_accessed via raw SQL.
func insertWithLastAccessed(t *testing.T, store *VectorStore, id, filePath, content, kind, sourceType string, lastAccessed int64) {
	t.Helper()
	vec := EncodeFloat32Vec([]float32{1.0})
	_, err := store.DB().Exec(`
		INSERT INTO entries (id, file_path, start_line, end_line, content, kind, identifier, vector, mod_time, content_hash, source_type, metadata, relationships, last_accessed)
		VALUES (?, ?, 0, 0, ?, ?, '', ?, 100, '', ?, '{}', '[]', ?)`,
		id, filePath, content, kind, vec, sourceType, lastAccessed)
	if err != nil {
		t.Fatalf("insertWithLastAccessed(%s): %v", id, err)
	}
}

func TestLifecycle_Archive(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()
	// Entry accessed 45 days ago (between active and archive thresholds)
	// with long content — should be archived
	longContent := strings.Repeat("x", 300)
	insertWithLastAccessed(t, store, "ext1", "JIRA-1", longContent, "ticket", "ticket", now-45*86400)

	// Entry accessed 45 days ago but with short content — should NOT be modified
	insertWithLastAccessed(t, store, "ext2", "JIRA-2", "short", "ticket", "ticket", now-45*86400)

	// Entry accessed 10 days ago — still active, should NOT be archived
	insertWithLastAccessed(t, store, "ext3", "JIRA-3", longContent, "ticket", "ticket", now-10*86400)

	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Archived != 1 {
		t.Errorf("Archived = %d, want 1", result.Archived)
	}

	// Verify the archived entry's content was truncated
	var content string
	store.DB().QueryRow(`SELECT content FROM entries WHERE id = ?`, "ext1").Scan(&content)
	if !strings.HasSuffix(content, "... [archived]") {
		t.Errorf("expected archived suffix, got %q", content)
	}
	if len(content) != 200+len("... [archived]") {
		t.Errorf("archived content length = %d, want %d", len(content), 200+len("... [archived]"))
	}

	// Verify the short entry was NOT modified
	store.DB().QueryRow(`SELECT content FROM entries WHERE id = ?`, "ext2").Scan(&content)
	if content != "short" {
		t.Errorf("short entry content = %q, want %q", content, "short")
	}

	// Verify the active entry was NOT modified
	store.DB().QueryRow(`SELECT content FROM entries WHERE id = ?`, "ext3").Scan(&content)
	if content != longContent {
		t.Errorf("active entry content was modified")
	}
}

func TestLifecycle_Prune(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()
	// Entry accessed 100 days ago — should be pruned
	insertWithLastAccessed(t, store, "old1", "JIRA-OLD", "old content", "ticket", "ticket", now-100*86400)

	// Entry accessed 50 days ago — between active and archive, should NOT be pruned
	insertWithLastAccessed(t, store, "mid1", "JIRA-MID", "mid content", "ticket", "ticket", now-50*86400)

	// Entry accessed 10 days ago — active, should NOT be pruned
	insertWithLastAccessed(t, store, "new1", "JIRA-NEW", "new content", "ticket", "ticket", now-10*86400)

	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1", result.Pruned)
	}

	// Verify the old entry is gone
	var count int
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries WHERE id = ?`, "old1").Scan(&count)
	if count != 0 {
		t.Error("pruned entry should be deleted")
	}

	// Verify others remain
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries WHERE id = ?`, "mid1").Scan(&count)
	if count != 1 {
		t.Error("mid-age entry should remain")
	}
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries WHERE id = ?`, "new1").Scan(&count)
	if count != 1 {
		t.Error("new entry should remain")
	}
}

func TestLifecycle_CodeAndMemoryExempt(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()
	longContent := strings.Repeat("x", 300)

	// Code entry accessed 100 days ago — should NOT be pruned or archived
	insertWithLastAccessed(t, store, "code1", "main.go", longContent, "function", "code", now-100*86400)

	// Memory entry accessed 100 days ago — should NOT be pruned or archived
	insertWithLastAccessed(t, store, "mem1", "memory-1", longContent, "memory", "doc", now-100*86400)

	// Ticket entry accessed 100 days ago — should be pruned
	insertWithLastAccessed(t, store, "ticket1", "JIRA-1", "old ticket", "ticket", "ticket", now-100*86400)

	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Only the ticket should be pruned
	if result.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1", result.Pruned)
	}

	// Code entry should remain unchanged
	var content string
	store.DB().QueryRow(`SELECT content FROM entries WHERE id = ?`, "code1").Scan(&content)
	if content != longContent {
		t.Error("code entry content should be unchanged")
	}

	// Memory entry should remain unchanged
	store.DB().QueryRow(`SELECT content FROM entries WHERE id = ?`, "mem1").Scan(&content)
	if content != longContent {
		t.Error("memory entry content should be unchanged")
	}
}

func TestLifecycle_SizeCap(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()

	// Insert 15 non-exempt entries with varying last_accessed
	for i := 0; i < 15; i++ {
		id := "ext" + string(rune('a'+i))
		insertWithLastAccessed(t, store, id, "JIRA-"+id, "content", "ticket", "ticket", now-int64(i)*86400)
	}

	// Insert 3 code entries (exempt from cap)
	for i := 0; i < 3; i++ {
		id := "code" + string(rune('a'+i))
		insertWithLastAccessed(t, store, id, id+".go", "code content", "function", "code", now)
	}

	// Total = 18, cap = 10. Excess = 8. Only non-exempt can be deleted.
	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Capped != 8 {
		t.Errorf("Capped = %d, want 8", result.Capped)
	}

	// Verify total count is now 10
	var total int
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&total)
	if total != 10 {
		t.Errorf("total entries = %d, want 10", total)
	}

	// Verify all code entries survived
	var codeCount int
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries WHERE source_type = 'code'`).Scan(&codeCount)
	if codeCount != 3 {
		t.Errorf("code entries = %d, want 3", codeCount)
	}
}

func TestLifecycle_SizeCapExemptEntries(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()

	// Insert 12 code entries — all exempt
	for i := 0; i < 12; i++ {
		id := "code" + string(rune('a'+i))
		insertWithLastAccessed(t, store, id, id+".go", "code", "function", "code", now)
	}

	// Cap is 10 but all entries are exempt — none should be deleted
	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Capped != 0 {
		t.Errorf("Capped = %d, want 0 (all exempt)", result.Capped)
	}

	var total int
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&total)
	if total != 12 {
		t.Errorf("total entries = %d, want 12 (exempt entries not capped)", total)
	}
}

func TestLifecycle_NeverAccessedNotAffected(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	longContent := strings.Repeat("x", 300)

	// Entry with last_accessed = 0 (never accessed) — should NOT be archived or pruned
	insertWithLastAccessed(t, store, "never1", "JIRA-NEVER", longContent, "ticket", "ticket", 0)

	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Archived != 0 {
		t.Errorf("Archived = %d, want 0", result.Archived)
	}
	if result.Pruned != 0 {
		t.Errorf("Pruned = %d, want 0", result.Pruned)
	}

	// Verify content is unchanged
	var content string
	store.DB().QueryRow(`SELECT content FROM entries WHERE id = ?`, "never1").Scan(&content)
	if content != longContent {
		t.Error("never-accessed entry should not be modified")
	}
}

func TestLifecycle_CombinedStages(t *testing.T) {
	dir := tempStoreDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Unix()
	longContent := strings.Repeat("x", 300)

	// 1 entry to archive (45 days old, long content)
	insertWithLastAccessed(t, store, "arch1", "JIRA-ARCH", longContent, "ticket", "ticket", now-45*86400)

	// 1 entry to prune (100 days old)
	insertWithLastAccessed(t, store, "prune1", "JIRA-PRUNE", "old stuff", "ticket", "ticket", now-100*86400)

	// 1 active entry
	insertWithLastAccessed(t, store, "active1", "JIRA-ACTIVE", "active stuff", "ticket", "ticket", now-5*86400)

	// 1 code entry (exempt)
	insertWithLastAccessed(t, store, "code1", "main.go", "code", "function", "code", now-200*86400)

	cfg := LifecycleConfig{ActiveDays: 30, ArchiveDays: 90, MaxChunks: 10000}
	result, err := RunLifecycle(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if result.Archived != 1 {
		t.Errorf("Archived = %d, want 1", result.Archived)
	}
	if result.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1", result.Pruned)
	}
	if result.Capped != 0 {
		t.Errorf("Capped = %d, want 0", result.Capped)
	}

	// 3 entries should remain: archived, active, code
	var total int
	store.DB().QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&total)
	if total != 3 {
		t.Errorf("total entries = %d, want 3", total)
	}
}
