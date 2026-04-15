package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caio-silva/heimdall-mcp/internal/config"
)

// ----- foreground path tests -----

// newForegroundDeps builds a PostEditDeps that records spawns into a counter
// and uses the TTL-stamp lockfile fallback (so tests don't depend on flock
// semantics on the host filesystem).
func newForegroundDeps(now time.Time, spawnCounter *int, spawnErr error) PostEditDeps {
	return PostEditDeps{
		Now: func() time.Time { return now },
		Spawn: func(projectRoot string) error {
			*spawnCounter++
			return spawnErr
		},
		UseFlock: false,
		KillCheck: func(pid int) bool {
			return false
		},
	}
}

func eventJSON(filePath string) string {
	return fmt.Sprintf(`{"tool_name":"Edit","tool_input":{"file_path":%q}}`, filePath)
}

func TestHookPostEdit_HappyFirstFire_SpawnsActor(t *testing.T) {
	tmp := t.TempDir()
	edited := filepath.Join(tmp, "a.go")
	if err := os.WriteFile(edited, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	deps := newForegroundDeps(time.Unix(1_700_000_000, 0), &spawned, nil)
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(edited)),
		&bytes.Buffer{}, &bytes.Buffer{},
		nil,
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d; want 0", rc)
	}
	if spawned != 1 {
		t.Fatalf("spawn count=%d; want 1", spawned)
	}
	// Pending file exists and contains the edited path.
	pending, err := os.ReadFile(filepath.Join(tmp, ".heimdall_db", "hooks", "reindex.pending"))
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if !strings.Contains(string(pending), edited) {
		t.Fatalf("pending missing %q: %q", edited, pending)
	}
}

func TestHookPostEdit_CoalesceWindow_NoRespawn(t *testing.T) {
	tmp := t.TempDir()
	// Seed a recent last_run so the coalesce window blocks a respawn.
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	if err := writeLastRun(filepath.Join(hooksDir, "reindex.last_run"), now.Add(-1*time.Second)); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	deps := newForegroundDeps(now, &spawned, nil)
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(filepath.Join(tmp, "b.go"))),
		&bytes.Buffer{}, &bytes.Buffer{},
		nil,
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if spawned != 0 {
		t.Fatalf("spawn count=%d; want 0 (coalesced)", spawned)
	}
	// Pending should still include the new file — coalesced fires still
	// enqueue so the running actor picks them up.
	data, _ := os.ReadFile(filepath.Join(hooksDir, "reindex.pending"))
	if !strings.Contains(string(data), "b.go") {
		t.Fatalf("pending missing enqueued file: %q", data)
	}
}

func TestHookPostEdit_LockHeld_NoWork(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-occupy the lockfile with a fresh stamp so the fallback path
	// treats it as held.
	lockPath := filepath.Join(hooksDir, "reindex.lock")
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.WriteFile(lockPath, []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	deps := newForegroundDeps(time.Now(), &spawned, nil)
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(filepath.Join(tmp, "c.go"))),
		&bytes.Buffer{}, &bytes.Buffer{},
		nil,
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if spawned != 0 {
		t.Fatalf("spawn count=%d; want 0", spawned)
	}
}

func TestHookPostEdit_HooksDisabledEnv(t *testing.T) {
	tmp := t.TempDir()
	spawned := 0
	deps := newForegroundDeps(time.Now(), &spawned, nil)
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(filepath.Join(tmp, "d.go"))),
		&bytes.Buffer{}, &bytes.Buffer{},
		map[string]string{"HEIMDALL_HOOKS": "0"},
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if spawned != 0 {
		t.Fatalf("spawn count=%d; want 0 (disabled)", spawned)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".heimdall_db")); !os.IsNotExist(err) {
		t.Fatalf(".heimdall_db was created under HEIMDALL_HOOKS=0")
	}
}

func TestHookPostEdit_OrphanedPID_SpawnsFresh(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a PID file for a process the KillCheck fake will declare dead.
	if err := writePID(filepath.Join(hooksDir, "reindex.inflight.pid"), 999999); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	deps := newForegroundDeps(time.Unix(1_700_000_000, 0), &spawned, nil)
	// Default KillCheck already returns false — orphan.
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(filepath.Join(tmp, "e.go"))),
		&bytes.Buffer{}, &bytes.Buffer{},
		nil,
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if spawned != 1 {
		t.Fatalf("spawn count=%d; want 1 (after orphan reap)", spawned)
	}
	// Orphan PID file should be cleared after reap.
	if _, err := os.Stat(filepath.Join(hooksDir, "reindex.inflight.pid")); !os.IsNotExist(err) {
		t.Fatalf("inflight.pid still present after orphan reap")
	}
}

func TestHookPostEdit_FallbackLockPath(t *testing.T) {
	// Explicitly exercise the O_CREAT|O_EXCL TTL-stamp fallback by setting
	// UseFlock: false, then verify a happy spawn still occurs.
	tmp := t.TempDir()
	spawned := 0
	deps := newForegroundDeps(time.Unix(1_700_000_000, 0), &spawned, nil)
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(filepath.Join(tmp, "f.go"))),
		&bytes.Buffer{}, &bytes.Buffer{},
		nil,
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if spawned != 1 {
		t.Fatalf("spawn count=%d; want 1", spawned)
	}
	// After release, lockfile should be gone (fallback deletes on release).
	if _, err := os.Stat(filepath.Join(tmp, ".heimdall_db", "hooks", "reindex.lock")); !os.IsNotExist(err) {
		t.Fatalf("lockfile not cleaned up")
	}
}

func TestAppendPending_CapsAtMax(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "reindex.pending")
	// Seed 1001 lines.
	var b strings.Builder
	for i := 0; i < 1001; i++ {
		b.WriteString("/seed/")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendPending(path, "/new/edit.go", 1000); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1000 {
		t.Fatalf("line count=%d; want 1000", len(lines))
	}
	if lines[0] != "/seed/2" {
		// After dropping 2 oldest (to make room for newcomer and keep 1000)
		// the oldest surviving entry is seed/2.
		t.Fatalf("oldest line=%q; want /seed/2 (two oldest should have been dropped)", lines[0])
	}
	if lines[len(lines)-1] != "/new/edit.go" {
		t.Fatalf("newest line=%q; want /new/edit.go", lines[len(lines)-1])
	}
}

func TestHookPostEdit_BadStdin_LogsAndExitsZero(t *testing.T) {
	tmp := t.TempDir()
	spawned := 0
	deps := newForegroundDeps(time.Now(), &spawned, nil)
	rc := hookPostEditWithDeps(
		config.Config{},
		strings.NewReader("not-json"),
		&bytes.Buffer{}, &bytes.Buffer{},
		nil,
		[]string{"--project", tmp},
		deps,
	)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if spawned != 0 {
		t.Fatalf("spawn count=%d; want 0 (no file to enqueue)", spawned)
	}
}

func TestHookPostEdit_ForegroundLatencyUnder50ms(t *testing.T) {
	// Sanity check that the foreground path meets the §3.3 budget on a
	// warm filesystem. Not a perf gate — just a smoke test so a future
	// change that accidentally adds an Ollama call or store open gets
	// caught in CI.
	tmp := t.TempDir()
	spawned := 0
	deps := newForegroundDeps(time.Unix(1_700_000_000, 0), &spawned, nil)
	start := time.Now()
	_ = hookPostEditWithDeps(
		config.Config{},
		strings.NewReader(eventJSON(filepath.Join(tmp, "z.go"))),
		io.Discard, io.Discard,
		nil,
		[]string{"--project", tmp},
		deps,
	)
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("foreground took %v; want ≤50ms", elapsed)
	}
}

// ----- actor tests -----

// fakeIndexRunner captures Reindex calls and can be scripted to fail on
// specific files for deadletter testing.
type fakeIndexRunner struct {
	calls     int
	lastFiles []string
	failOn    map[string]bool
	failAll   bool
}

func (f *fakeIndexRunner) Reindex(ctx context.Context, files []string) error {
	f.calls++
	f.lastFiles = append([]string(nil), files...)
	if f.failAll {
		return errors.New("index_incremental: boom")
	}
	for _, file := range files {
		if f.failOn[file] {
			return errors.New("index_incremental: failed on " + file)
		}
	}
	return nil
}

func newActorCfg(hooksDir string, runner indexRunner, now time.Time) postEditActorConfig {
	return postEditActorConfig{
		projectRoot:    filepath.Dir(filepath.Dir(hooksDir)),
		hooksDir:       hooksDir,
		coalesceWindow: 1 * time.Millisecond,
		deadletterCap:  postEditDeadletterCap,
		now:            func() time.Time { return now },
		sleep:          func(time.Duration) {},
		runner:         runner,
	}
}

func TestRunPostEditActor_HappyDrain(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed pending with two files.
	if err := os.WriteFile(filepath.Join(hooksDir, "reindex.pending"), []byte("/a\n/b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexRunner{}
	now := time.Unix(1_700_000_000, 0)
	_ = runPostEditActor(context.Background(), newActorCfg(hooksDir, runner, now))

	if runner.calls != 1 {
		t.Fatalf("runner.calls=%d; want 1", runner.calls)
	}
	if len(runner.lastFiles) != 2 {
		t.Fatalf("lastFiles=%v; want 2", runner.lastFiles)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "reindex.pending")); !os.IsNotExist(err) {
		t.Fatalf("reindex.pending not drained")
	}
	lr := readLastRun(filepath.Join(hooksDir, "reindex.last_run"))
	if !lr.Equal(now) {
		t.Fatalf("last_run=%v; want %v", lr, now)
	}
}

func TestRunPostEditActor_IndexerError_WritesDeadletter(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "reindex.pending"), []byte("/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexRunner{failAll: true}
	_ = runPostEditActor(context.Background(), newActorCfg(hooksDir, runner, time.Unix(1_700_000_000, 0)))

	entries, err := readDeadletter(filepath.Join(hooksDir, "reindex.deadletter.jsonl"))
	if err != nil {
		t.Fatalf("read deadletter: %v", err)
	}
	if len(entries) != 1 || entries[0].File != "/x" || entries[0].Attempt != 1 {
		t.Fatalf("deadletter entries=%+v", entries)
	}
	if entries[0].ErrCode != "index_failed" {
		t.Fatalf("err_code=%q; want index_failed", entries[0].ErrCode)
	}
}

func TestRunPostEditActor_DeadletterRetry_Succeeds(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a deadletter entry that the runner will now accept.
	seed := []deadletterEntry{{File: "/recovered", ErrCode: "index_failed", Attempt: 2, TS: 1}}
	if err := writeDeadletter(filepath.Join(hooksDir, "reindex.deadletter.jsonl"), seed); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexRunner{} // accepts everything
	_ = runPostEditActor(context.Background(), newActorCfg(hooksDir, runner, time.Unix(1_700_000_000, 0)))

	// Deadletter file should be gone after successful retry.
	if _, err := os.Stat(filepath.Join(hooksDir, "reindex.deadletter.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("deadletter still present after recovery")
	}
}

func TestRunPostEditActor_DeadletterCap_HardDrops(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a deadletter entry at the cap — should be hard-dropped.
	seed := []deadletterEntry{{File: "/stuck", ErrCode: "index_failed", Attempt: postEditDeadletterCap, TS: 1}}
	if err := writeDeadletter(filepath.Join(hooksDir, "reindex.deadletter.jsonl"), seed); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexRunner{failAll: true}
	_ = runPostEditActor(context.Background(), newActorCfg(hooksDir, runner, time.Unix(1_700_000_000, 0)))

	// The capped entry should be gone even though the runner still fails.
	// (No pending means runner is only called during the retry pass, and
	// the capped entry is dropped before retry.)
	entries, _ := readDeadletter(filepath.Join(hooksDir, "reindex.deadletter.jsonl"))
	for _, e := range entries {
		if e.File == "/stuck" {
			t.Fatalf("capped entry still present: %+v", e)
		}
	}
	if runner.calls != 0 {
		t.Fatalf("runner called for capped entry: %d calls", runner.calls)
	}
}

func TestRunPostEditActor_NoPending_NoCall(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexRunner{}
	_ = runPostEditActor(context.Background(), newActorCfg(hooksDir, runner, time.Unix(1_700_000_000, 0)))
	if runner.calls != 0 {
		t.Fatalf("runner.calls=%d; want 0 for empty pending", runner.calls)
	}
	// last_run should NOT be bumped when there's nothing to do.
	if !readLastRun(filepath.Join(hooksDir, "reindex.last_run")).IsZero() {
		t.Fatalf("last_run bumped with nothing to reindex")
	}
}

func TestRunPostEditActor_InflightPIDCleared(t *testing.T) {
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, ".heimdall_db", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = runPostEditActor(context.Background(), newActorCfg(hooksDir, &fakeIndexRunner{}, time.Unix(1_700_000_000, 0)))
	if _, err := os.Stat(filepath.Join(hooksDir, "reindex.inflight.pid")); !os.IsNotExist(err) {
		t.Fatalf("inflight.pid not cleared on actor exit")
	}
}

func TestParsePostEditFlags_Defaults(t *testing.T) {
	f, err := parsePostEditFlags([]string{"--source=heimdall", "--version=1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.source != "heimdall" || f.version != 1 {
		t.Fatalf("flags=%+v", f)
	}
}

func TestClassifyErr(t *testing.T) {
	cases := map[string]string{
		"ollama_ping: down":          "ollama_down",
		"open_store: perm":           "store_open",
		"index_incremental: boom":    "index_failed",
		"wat":                        "unknown",
	}
	for in, want := range cases {
		got := classifyErr(errors.New(in))
		if got != want {
			t.Fatalf("classifyErr(%q)=%q; want %q", in, got, want)
		}
	}
}

// ----- dispatcher smoke tests -----

func TestDispatchHook_NoSubcommand_ExitZero(t *testing.T) {
	code := DispatchHook(config.Config{}, nil, &bytes.Buffer{}, &bytes.Buffer{}, nil, nil)
	if code != 0 {
		t.Fatalf("code=%d; want 0", code)
	}
}

func TestDispatchHook_UnknownSubcommand_ExitZero(t *testing.T) {
	code := DispatchHook(config.Config{}, nil, &bytes.Buffer{}, &bytes.Buffer{}, nil, []string{"nope"})
	if code != 0 {
		t.Fatalf("code=%d; want 0", code)
	}
}

func TestDispatchHook_PostEdit_NoStdin_ExitZero(t *testing.T) {
	tmp := t.TempDir()
	code := DispatchHook(
		config.Config{},
		strings.NewReader(""),
		&bytes.Buffer{}, &bytes.Buffer{},
		map[string]string{"HEIMDALL_HOOKS": "0"},
		[]string{"post-edit", "--project", tmp, "--source=heimdall", "--version=1"},
	)
	if code != 0 {
		t.Fatalf("code=%d; want 0", code)
	}
}
