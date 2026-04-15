package heimdall

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *VectorStore {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestHookCachePutGet_RoundTrip covers the happy-path put followed by a get
// that returns the same bytes and increments hit_count.
func TestHookCachePutGet_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("## Heimdall suggests\n### foo:L1-L3 (score 0.99)\n```\nsnippet\n```\n")

	if err := store.HookCachePut("k1", payload, time.Minute, 100); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.HookCacheGet("k1", time.Minute)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %q want %q", got, payload)
	}

	// Second get bumps hit_count to 2.
	if _, err := store.HookCacheGet("k1", time.Minute); err != nil {
		t.Fatalf("second get: %v", err)
	}
	count, _, hits, err := store.HookCacheStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if count != 1 {
		t.Errorf("count: got %d want 1", count)
	}
	if hits != 2 {
		t.Errorf("hit_count: got %d want 2", hits)
	}
}

func TestHookCacheGet_Miss(t *testing.T) {
	store := newTestStore(t)
	_, err := store.HookCacheGet("absent", time.Minute)
	if !errors.Is(err, ErrHookCacheMiss) {
		t.Errorf("expected ErrHookCacheMiss, got %v", err)
	}
}

func TestHookCacheGet_TTLExpiry(t *testing.T) {
	store := newTestStore(t)
	if err := store.HookCachePut("k", []byte("payload"), 0, 0); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Rewind created_at 2 hours into the past.
	if _, err := store.db.Exec(
		`UPDATE hook_cache SET created_at = ? WHERE key = ?`,
		time.Now().Add(-2*time.Hour).Unix(), "k",
	); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	// 1-hour TTL ⇒ expired.
	if _, err := store.HookCacheGet("k", time.Hour); !errors.Is(err, ErrHookCacheMiss) {
		t.Errorf("expected ErrHookCacheMiss on expiry, got %v", err)
	}
	// 0 TTL ⇒ no freshness gate, hit.
	if got, err := store.HookCacheGet("k", 0); err != nil || !bytes.Equal(got, []byte("payload")) {
		t.Errorf("expected hit with ttl=0, got %v / %q", err, got)
	}
}

func TestHookCachePut_OversizeRefusal(t *testing.T) {
	store := newTestStore(t)
	big := make([]byte, MaxHookCacheStdoutBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	err := store.HookCachePut("k", big, time.Minute, 100)
	if !errors.Is(err, ErrHookCacheOversize) {
		t.Errorf("expected ErrHookCacheOversize, got %v", err)
	}
	// Nothing should have been stored.
	if _, err := store.HookCacheGet("k", time.Minute); !errors.Is(err, ErrHookCacheMiss) {
		t.Errorf("oversize must not persist; got %v", err)
	}
}

func TestHookCachePut_RowCapEviction(t *testing.T) {
	store := newTestStore(t)

	// Insert 5 rows with monotonically increasing created_at (sleep 10ms
	// between each so eviction ordering is deterministic at 1-second
	// granularity... use manual created_at rewind instead).
	for i := 0; i < 5; i++ {
		if err := store.HookCachePut(fmt.Sprintf("k%d", i), []byte("x"), time.Minute, 0); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		// Force an ordered created_at.
		if _, err := store.db.Exec(
			`UPDATE hook_cache SET created_at = ? WHERE key = ?`,
			int64(1000+i), fmt.Sprintf("k%d", i),
		); err != nil {
			t.Fatalf("rewind: %v", err)
		}
	}

	// Now put a 6th row with cap=3 → should trigger eviction down to 3.
	// k5 gets a fresh created_at (time.Now()); k0..k4 are all older, so
	// eviction takes the three smallest created_at values: k0, k1, k2.
	if err := store.HookCachePut("k5", []byte("x"), time.Minute, 3); err != nil {
		t.Fatalf("put k5: %v", err)
	}
	count, _, _, _ := store.HookCacheStats()
	if count != 3 {
		t.Errorf("row count after eviction: got %d want 3", count)
	}
	// Use ttl=0 so the rewound created_at values don't trip the freshness
	// gate — this test cares only about row-cap eviction order.
	for _, gone := range []string{"k0", "k1", "k2"} {
		if _, err := store.HookCacheGet(gone, 0); !errors.Is(err, ErrHookCacheMiss) {
			t.Errorf("%s should have been evicted, got %v", gone, err)
		}
	}
	for _, kept := range []string{"k3", "k4", "k5"} {
		if _, err := store.HookCacheGet(kept, 0); err != nil {
			t.Errorf("%s should remain, got %v", kept, err)
		}
	}
}

func TestHookCacheClear(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 3; i++ {
		store.HookCachePut(fmt.Sprintf("k%d", i), []byte("v"), time.Minute, 0)
	}
	if err := store.HookCacheClear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	count, _, _, _ := store.HookCacheStats()
	if count != 0 {
		t.Errorf("expected 0 rows after clear, got %d", count)
	}
}

func TestIndexVersion_BumpsOnUpsert(t *testing.T) {
	store := newTestStore(t)
	v0 := store.GetIndexVersion()
	if v0 != 0 {
		t.Errorf("fresh store: got %d want 0", v0)
	}

	rec := VectorRecord{
		ID: "a", FilePath: "foo.go", Content: "hello", Embedding: []float32{1, 0, 0}, ModTime: 1,
	}
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if v := store.GetIndexVersion(); v != 1 {
		t.Errorf("after upsert: got %d want 1", v)
	}

	rec.Content = "world"
	if err := store.Upsert([]VectorRecord{rec}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if v := store.GetIndexVersion(); v != 2 {
		t.Errorf("after second upsert: got %d want 2", v)
	}
}

func TestIndexVersion_BumpsOnRemoveByFile(t *testing.T) {
	store := newTestStore(t)
	store.Upsert([]VectorRecord{{
		ID: "a", FilePath: "foo.go", Content: "x", Embedding: []float32{1}, ModTime: 1,
	}})
	before := store.GetIndexVersion()
	if err := store.RemoveByFile("foo.go"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := store.GetIndexVersion()
	if after != before+1 {
		t.Errorf("remove bump: got %d want %d", after, before+1)
	}
}

func TestIndexVersion_MonotonicUnderConcurrentUpserts(t *testing.T) {
	store := newTestStore(t)

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			store.Upsert([]VectorRecord{{
				ID:        fmt.Sprintf("id-%d", i),
				FilePath:  fmt.Sprintf("f-%d.go", i),
				Content:   "x",
				Embedding: []float32{float32(i)},
				ModTime:   int64(i),
			}})
		}(i)
	}
	wg.Wait()

	v := store.GetIndexVersion()
	if v != int64(N) {
		t.Errorf("concurrent bumps: got %d want %d", v, N)
	}
}

// Open an existing store twice (simulate upgrade) and confirm the schema
// migrates cleanly and retains its data / metadata.
func TestOpenStore_MigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	store1, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	store1.SetMetadata("embedding_model", "nomic-embed-text")
	store1.Upsert([]VectorRecord{{
		ID: "id1", FilePath: "foo.go", Content: "x", Embedding: []float32{1, 2}, ModTime: 1,
	}})
	if err := store1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer store2.Close()

	if m := store2.GetMetadata("embedding_model"); m != "nomic-embed-text" {
		t.Errorf("lost metadata: %q", m)
	}
	if !store2.HasFile("foo.go") {
		t.Error("lost data after reopen")
	}
	if v := store2.GetIndexVersion(); v != 1 {
		t.Errorf("index version after reopen: got %d want 1", v)
	}
}

// TestOpenStore_MigratesFromLegacySchema seeds a DB that lacks the
// hook_cache table and verifies OpenStore adds it without clobbering data.
func TestOpenStore_MigratesFromLegacySchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "vectors.db")

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE entries (
			id TEXT PRIMARY KEY,
			file_path TEXT NOT NULL,
			start_line INTEGER,
			end_line INTEGER,
			content TEXT NOT NULL,
			kind TEXT,
			identifier TEXT,
			vector BLOB NOT NULL,
			mod_time INTEGER NOT NULL
		);
		CREATE TABLE store_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO store_metadata (key, value) VALUES ('embedding_model', 'legacy-model');
	`); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	db.Close()

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer store.Close()

	if m := store.GetMetadata("embedding_model"); m != "legacy-model" {
		t.Errorf("lost legacy metadata: %q", m)
	}
	// hook_cache must exist and be usable.
	if err := store.HookCachePut("k", []byte("x"), time.Minute, 0); err != nil {
		t.Errorf("hook_cache unusable after migration: %v", err)
	}
}

// TestHookCacheGet_ZeroTTL pins the documented contract for ttl=0: the
// `if ttl > 0` guard in HookCacheGet skips the freshness check entirely,
// so ttl=0 means "don't gate on age — always return the row if it
// exists." A row written this instant must therefore come back as a hit.
func TestHookCacheGet_ZeroTTL(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("zero-ttl-payload")
	if err := store.HookCachePut("zt", payload, 0, 0); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.HookCacheGet("zt", 0)
	if err != nil {
		t.Fatalf("ttl=0 should not gate freshness, got %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %q want %q", got, payload)
	}

	// And the same semantics hold for a row aged into the distant past:
	// ttl=0 must still return it.
	if _, err := store.db.Exec(
		`UPDATE hook_cache SET created_at = ? WHERE key = ?`,
		time.Now().Add(-365*24*time.Hour).Unix(), "zt",
	); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if _, err := store.HookCacheGet("zt", 0); err != nil {
		t.Errorf("ttl=0 on ancient row should still hit, got %v", err)
	}
}

// TestHookCache_ConcurrentClearDuringGet exercises the two-phase lock in
// HookCacheGet against a concurrent HookCacheClear. Under `-race` this
// asserts there is no data race, no panic, and that every Get either
// returns the exact stored payload or ErrHookCacheMiss — never garbage.
func TestHookCache_ConcurrentClearDuringGet(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("concurrent-payload")

	// A writer goroutine keeps re-putting the row so clears don't
	// permanently starve the readers.
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.HookCachePut("ck", payload, time.Minute, 0)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.HookCacheClear()
		}
	}()

	const readers = 4
	const iterations = 250
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				got, err := store.HookCacheGet("ck", time.Minute)
				if err != nil {
					if !errors.Is(err, ErrHookCacheMiss) {
						t.Errorf("unexpected error: %v", err)
						return
					}
					continue
				}
				if !bytes.Equal(got, payload) {
					t.Errorf("garbage payload: got %q want %q", got, payload)
					return
				}
			}
		}()
	}

	// Cap the run so a slow CI box can't hang.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	close(stop)
	wg.Wait()
}

// TestHookCachePut_BinaryStdout asserts the BLOB column round-trips
// arbitrary bytes — NUL, 0xFF, and multi-byte Unicode — byte-for-byte.
func TestHookCachePut_BinaryStdout(t *testing.T) {
	store := newTestStore(t)
	payload := []byte{
		0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd,
		'h', 'e', 'l', 'l', 'o',
		0xe2, 0x98, 0x83, // ☃ snowman
		0xf0, 0x9f, 0x9a, 0x80, // 🚀 rocket
		0x00, 0x00,
	}

	if err := store.HookCachePut("bin", payload, time.Minute, 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.HookCacheGet("bin", time.Minute)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("binary round-trip failed:\n got: % x\nwant: % x", got, payload)
	}
}
