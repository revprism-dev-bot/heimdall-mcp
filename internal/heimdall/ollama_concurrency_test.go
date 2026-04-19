package heimdall

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------
// Tests for bounded parallelism, configurable timeout, and retry added in
// PR5 (closes handoff Problem #5 — parallel Ollama bursts deadline-exceed).
// -----------------------------------------------------------------------

// slowEmbedServer returns an httptest.Server whose /api/embed handler
// records in-flight concurrency (peak and current) and blocks until
// `release` is closed or ctx is done. The returned `started` channel
// carries one send per request actually landed in the handler — tests
// receive N times to wait DETERMINISTICALLY for all goroutines to be
// in-flight, instead of relying on time.Sleep-based oracles that are
// flaky under -race / loaded CI. See pr74-review-tests.md F7 / MF-3.
func slowEmbedServer(t *testing.T, release <-chan struct{}) (srv *httptest.Server, inflight, peak *int64, started <-chan struct{}) {
	t.Helper()
	var inflightV, peakV int64
	startedCh := make(chan struct{}, 1024) // generous so a burst never blocks
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		cur := atomic.AddInt64(&inflightV, 1)
		for {
			old := atomic.LoadInt64(&peakV)
			if cur <= old || atomic.CompareAndSwapInt64(&peakV, old, cur) {
				break
			}
		}
		// Announce that this request is in-flight BEFORE blocking so
		// the test can await N such signals.
		startedCh <- struct{}{}
		defer atomic.AddInt64(&inflightV, -1)

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &inflightV, &peakV, startedCh
}

// waitStarted pulls N signals off the `started` channel, or fails the
// test after `timeout`. Used to wait for all expected goroutines to
// land inside the server handler before asserting peak.
func waitStarted(t *testing.T, started <-chan struct{}, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatalf("timeout waiting for request %d/%d to land in server handler", i+1, n)
		}
	}
}

// TestOllamaSemaphore_BoundsConcurrency: a client constructed with cap=2
// must never allow more than 2 in-flight /api/embed requests, even when
// 10 goroutines call Embed concurrently.
func TestOllamaSemaphore_BoundsConcurrency(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 2)

	const N = 10
	const cap = 2
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.Embed(ctx, "m", "t")
			errs <- err
		}()
	}

	// Deterministically wait for `cap` requests to be in-flight in the
	// server handler — that is the observable condition under test. The
	// remaining N-cap goroutines are provably blocked on the semaphore
	// (if they weren't, peak would exceed cap — which we assert below).
	waitStarted(t, started, cap, 5*time.Second)

	// Now release the server. All requests will return.
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("embed: unexpected error %v", err)
		}
	}

	if got := atomic.LoadInt64(peak); got > cap {
		t.Fatalf("peak in-flight = %d, want <= %d", got, cap)
	}
	if got := atomic.LoadInt64(peak); got < 1 {
		t.Fatalf("peak in-flight = %d, want >= 1 (sanity)", got)
	}
}

// TestOllamaClient_BackwardCompatConstructor: NewOllamaClient must continue
// to work and must apply the default cap (>0), not unbounded. Callers of
// the original constructor do not opt in to concurrency explicitly so we
// enforce the safe default.
func TestOllamaClient_BackwardCompatConstructor(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClient(srv.URL)

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	waitStarted(t, started, DefaultEmbedMaxConcurrent, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got > int64(DefaultEmbedMaxConcurrent) {
		t.Fatalf("peak in-flight = %d, want <= %d (default cap)", got, DefaultEmbedMaxConcurrent)
	}
}

// TestOllamaClient_UnboundedWhenCapZero: passing cap=0 disables the
// semaphore (explicit unbounded mode). Useful for callers that manage
// their own concurrency upstream.
func TestOllamaClient_UnboundedWhenCapZero(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 0)

	const N = 6
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	// Unbounded → all N goroutines must land in the handler.
	waitStarted(t, started, N, 5*time.Second)
	if got := atomic.LoadInt64(peak); got < int64(N) {
		t.Fatalf("peak in-flight = %d, want >= %d (unbounded cap=0)", got, N)
	}
	close(release)
	wg.Wait()
}

// TestOllamaClient_NegativeCapRejected: a negative cap is an invalid
// configuration. The constructor returns nil client? No — we make it
// coerce to default and log. Rationale: constructor cannot return an
// error without breaking every existing caller's signature.
func TestOllamaClient_NegativeCapCoercesToDefault(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)
	client := NewOllamaClientWithLimit(srv.URL, -5)

	const N = 6
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	// After coercion peak equals default cap.
	waitStarted(t, started, DefaultEmbedMaxConcurrent, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got > int64(DefaultEmbedMaxConcurrent) {
		t.Fatalf("peak in-flight = %d, want <= %d (negative cap coerces to default)", got, DefaultEmbedMaxConcurrent)
	}
}

// TestOllamaClient_SemaphoreRespectsCtxCancel: if the context is cancelled
// while a goroutine is waiting on the semaphore (cap exhausted), the call
// returns immediately with ctx.Err(), not after the semaphore frees.
func TestOllamaClient_SemaphoreRespectsCtxCancel(t *testing.T) {
	release := make(chan struct{})
	srv, _, _, started := slowEmbedServer(t, release)
	defer close(release)

	client := NewOllamaClientWithLimit(srv.URL, 1)

	// Occupy the sole slot with a goroutine that will block forever until
	// `release` closes (via defer above).
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.Embed(ctx, "m", "blocker")
	}()
	// Deterministic: wait until the blocker is actually holding the slot
	// (has reached the server handler, having already acquired the sema).
	waitStarted(t, started, 1, 5*time.Second)

	// Now call with a short ctx — must unblock via ctx cancel, not via
	// the semaphore (since the blocker still holds it).
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Embed(ctx, "m", "fast")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from ctx cancel, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	// Should be roughly the ctx deadline, not the blocker's deadline.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel took %v, want <500ms (semaphore wait did not respect ctx)", elapsed)
	}
}

// TestOllamaClient_ConfigurableConcurrency: an explicit cap passed via
// the new constructor overrides the default.
func TestOllamaClient_ConfigurableConcurrency(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 3)

	const N = 6
	const cap = 3
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	// Wait for `cap` goroutines to hit the handler; remaining must be
	// blocked on the semaphore.
	waitStarted(t, started, cap, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got > int64(cap) {
		t.Fatalf("peak in-flight = %d, want <= %d", got, cap)
	}
	if got := atomic.LoadInt64(peak); got < 2 {
		t.Fatalf("peak in-flight = %d, want >= 2 (cap=3 should allow bursting)", got)
	}
}

// TestOllamaClient_TimeoutConfigurable: setting embed.timeout_ms changes
// the default deadline applied when the caller's context has none.
func TestOllamaClient_TimeoutConfigurable(t *testing.T) {
	// Server that never responds until we tell it to, so the test
	// owns the release path deterministically.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  200 * time.Millisecond,
		MaxRetries:    0,
	})

	// No deadline on the caller ctx — the client's default must apply.
	start := time.Now()
	_, err := client.Embed(context.Background(), "m", "t")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("timeout took %v, want close to 200ms", elapsed)
	}
	// Lower bound guards against the timeout silently falling back to a
	// shorter default (e.g. 100ms) and the test passing spuriously.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("timeout fired in %v — configured 200ms was likely ignored", elapsed)
	}
	if !strings.Contains(err.Error(), "context") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline-exceeded error, got: %v", err)
	}
}

// TestOllamaClient_RetryOnDeadlineExceeded: first two attempts time out
// against a slow server; third succeeds. Retry path should deliver the
// success transparently to the caller.
func TestOllamaClient_RetryOnDeadlineExceeded(t *testing.T) {
	var attempts int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&attempts, 1)
		if n <= 2 {
			select {
			case <-done:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[0.5]]}`))
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  150 * time.Millisecond,
		MaxRetries:    2,
		RetryBackoff:  []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
	})

	// Caller ctx must be long enough for all attempts (3 * 150ms + backoff).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	vec, err := client.Embed(ctx, "m", "t")
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if len(vec) != 1 || vec[0] != 0.5 {
		t.Fatalf("unexpected vec: %v", vec)
	}
	if got := atomic.LoadInt64(&attempts); got != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 retries)", got)
	}
}

// TestOllamaClient_NoRetryOnNonTimeoutError: a 500 response is NOT
// retried. Retries are scoped strictly to deadline-exceeded errors.
func TestOllamaClient_NoRetryOnNonTimeoutError(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  500 * time.Millisecond,
		MaxRetries:    3,
		RetryBackoff:  []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
	})

	_, err := client.Embed(context.Background(), "m", "t")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if got := atomic.LoadInt64(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on non-timeout)", got)
	}
}

// TestOllamaClient_NoRetryOnCallerCtxCancel: if the caller's context
// deadline is exceeded (vs the per-request deadline), do not retry —
// the caller has asked us to stop.
func TestOllamaClient_NoRetryOnCallerCtxCancel(t *testing.T) {
	var attempts int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  5 * time.Second, // per-request is generous
		MaxRetries:    3,
		RetryBackoff:  []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Embed(ctx, "m", "t")
	if err == nil {
		t.Fatal("expected error")
	}
	// Caller ctx expired before per-request ctx, so only one attempt fires.
	if got := atomic.LoadInt64(&attempts); got > 1 {
		t.Fatalf("attempts = %d, want 1 (caller ctx cancel must not trigger retry)", got)
	}
}

// TestOllamaClient_CapOne_SingleFlight: cap=1 serializes all requests.
func TestOllamaClient_CapOne_SingleFlight(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 1)

	const N = 4
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	waitStarted(t, started, 1, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got != 1 {
		t.Fatalf("peak in-flight = %d, want 1 (cap=1)", got)
	}
}

// TestOllamaClient_EmbedForHook_AlsoBounded: the hook-path entry point
// shares the same semaphore. Regression: docs/plans/hooks §5.6 requires
// the hook path to stay hot; we must not unintentionally bypass bounding.
func TestOllamaClient_EmbedForHook_AlsoBounded(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 1)

	const N = 4
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.EmbedForHook(ctx, "m", "t")
		}()
	}
	waitStarted(t, started, 1, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got != 1 {
		t.Fatalf("EmbedForHook peak in-flight = %d, want 1 (shared semaphore, cap=1)", got)
	}
}

// TestOllamaClient_BackoffReusesLastEntry: when MaxRetries exceeds
// len(RetryBackoff), the last backoff entry is reused for remaining
// retries. Verified indirectly by confirming total elapsed time reflects
// the repeated entry.
func TestOllamaClient_BackoffReusesLastEntry(t *testing.T) {
	var attempts int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&attempts, 1)
		if n < 4 { // fail first 3 via deadline
			select {
			case <-done:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	// 3 retries but only 1 backoff entry — the entry repeats.
	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  50 * time.Millisecond,
		MaxRetries:    3,
		RetryBackoff:  []time.Duration{100 * time.Millisecond},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.Embed(ctx, "m", "t")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	// Min: 4×50ms (attempts) + 3×100ms (backoffs reused) = 500ms.
	if elapsed < 400*time.Millisecond {
		t.Fatalf("elapsed = %v, want >=400ms (backoff must repeat last entry)", elapsed)
	}
	if got := atomic.LoadInt64(&attempts); got != 4 {
		t.Fatalf("attempts = %d, want 4", got)
	}
}

// TestOllamaClient_EmbedBatch_AlsoBounded: the batch embed path must also
// respect the semaphore, not just the single-text path.
//
// Uses a dedicated batch-aware server (multi-embedding response shape)
// with the same event-driven started-signal pattern as slowEmbedServer.
func TestOllamaClient_EmbedBatch_AlsoBounded(t *testing.T) {
	release := make(chan struct{})
	var inflight, peak int64
	started := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		started <- struct{}{}
		defer atomic.AddInt64(&inflight, -1)

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 2-text batch response
		_, _ = w.Write([]byte(`{"embeddings":[[1.0],[0.5]]}`))
	}))
	t.Cleanup(srv.Close)

	client := NewOllamaClientWithLimit(srv.URL, 2)

	const N = 8
	const cap = 2
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.EmbedBatch(ctx, "m", []string{"a", "b"})
		}()
	}
	// Deterministic: wait for `cap` goroutines to hit the handler.
	for i := 0; i < cap; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for batch request %d/%d", i+1, cap)
		}
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > cap {
		t.Fatalf("peak in-flight = %d, want <= %d (batch must respect semaphore)", got, cap)
	}
}

// TestNewOllamaClientFromConfig_AppliesConfig: verifies the config-flow
// helper wires MaxConcurrent, EmbedTimeout, and MaxRetries through.
// After PR74 F3 fix: sentinel semantics are -1 = "use default",
// 0 = "explicit unbounded / zero retries", N > 0 = "literal".
func TestNewOllamaClientFromConfig_AppliesConfig(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	// maxConcurrent=1 literal, timeoutMs=0 (→ default), maxRetries=0
	// (→ default 2 per sentinel semantics).
	client := NewOllamaClientFromConfig(srv.URL, 1, 0, 0)

	const N = 4
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	waitStarted(t, started, 1, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got != 1 {
		t.Fatalf("peak in-flight = %d, want 1 (NewOllamaClientFromConfig maxConcurrent=1)", got)
	}
	if client.maxRetries != DefaultEmbedMaxRetries {
		t.Fatalf("maxRetries = %d, want %d (maxRetries=0 should fall through to default)", client.maxRetries, DefaultEmbedMaxRetries)
	}
	if client.embedTimeout != DefaultEmbedTimeout {
		t.Fatalf("embedTimeout = %v, want %v (timeoutMs=0 should fall through to default)", client.embedTimeout, DefaultEmbedTimeout)
	}
}

// TestNewOllamaClientFromConfig_NegativeRetriesCoercesToZero: the -1
// sentinel means "explicitly disable retries" (config JSON zero is
// ambiguous in the pre-review contract; post-F3 fix 0 now means "use
// default" since -1 is the unset sentinel).
func TestNewOllamaClientFromConfig_NegativeRetriesCoercesToZero(t *testing.T) {
	client := NewOllamaClientFromConfig("http://127.0.0.1:0", 2, 1000, -1)
	if client.maxRetries != 0 {
		t.Fatalf("maxRetries = %d, want 0 (negative sentinel → zero retries)", client.maxRetries)
	}
	if client.embedTimeout != time.Second {
		t.Fatalf("embedTimeout = %v, want 1s", client.embedTimeout)
	}
}

// TestOllamaClient_ZeroMeansUnbounded: regression for PR74 review F3.
// The README documents `embed_max_concurrent: 0` as "unbounded", but
// pre-fix NewOllamaClientFromConfig silently remapped 0 → default cap
// (2). After the fix: 0 yields a nil semaphore (literally unbounded).
// The sentinel for "use default" is now -1.
func TestOllamaClient_ZeroMeansUnbounded(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	// 0 must mean unbounded, per documented contract.
	client := NewOllamaClientFromConfig(srv.URL, 0, 0, 0)

	const N = 100 // large enough to trip any cap below N
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	// All N goroutines must land in the handler if unbounded.
	waitStarted(t, started, N, 10*time.Second)
	if got := atomic.LoadInt64(peak); got < int64(N) {
		t.Fatalf("peak in-flight = %d, want >= %d (0 must mean unbounded)", got, N)
	}
	close(release)
	wg.Wait()
}

// TestOllamaClient_SentinelMinusOneUsesDefault: the -1 sentinel yields
// the package default cap (DefaultEmbedMaxConcurrent=2). Complements
// TestOllamaClient_ZeroMeansUnbounded to pin the full sentinel contract.
func TestOllamaClient_SentinelMinusOneUsesDefault(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak, started := slowEmbedServer(t, release)

	client := NewOllamaClientFromConfig(srv.URL, -1, 0, -1)

	const N = 6
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = client.Embed(ctx, "m", "t")
		}()
	}
	waitStarted(t, started, DefaultEmbedMaxConcurrent, 5*time.Second)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got > int64(DefaultEmbedMaxConcurrent) {
		t.Fatalf("peak in-flight = %d, want <= %d (-1 must map to default)", got, DefaultEmbedMaxConcurrent)
	}
	// -1 for maxRetries means "explicitly disable retries" (0).
	if client.maxRetries != 0 {
		t.Fatalf("maxRetries = %d, want 0 (-1 must coerce to zero retries)", client.maxRetries)
	}
}

// TestOllamaClient_BackoffTimerReleasedOnCtxCancel: regression for F4.
// When the caller's ctx cancels during a retry backoff sleep, the
// timer must be stopped immediately (time.After leaks timers until
// their duration elapses). We verify by ensuring elapsed time is close
// to the ctx cancel, not the backoff duration.
func TestOllamaClient_BackoffTimerReleasedOnCtxCancel(t *testing.T) {
	var attempts int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		// Slow server so first attempt deadlines.
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	// Very long backoff — if timer leaked we'd wait the full duration
	// (2s) before returning, past the ctx deadline.
	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  50 * time.Millisecond,
		MaxRetries:    3,
		RetryBackoff:  []time.Duration{2 * time.Second},
	})

	// Caller ctx is 150ms — just enough for one attempt + part of
	// the first backoff; must unblock on ctx cancel, not on timer fire.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.Embed(ctx, "m", "t")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error (caller-ctx cancel)")
	}
	// Must return at or shortly after ctx deadline; a leaked timer
	// would pin us until the 2s backoff fired.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed=%v, want <500ms (backoff timer leaked — did NOT respond to ctx cancel)", elapsed)
	}
}

// TestOllamaClient_SemaphoreReleasedOnNonCtxErrors: regression for F9.
// The semaphore slot MUST be released on every embed exit path —
// including HTTP 500 and JSON decode errors. Without this guard a
// future refactor of the `defer release()` site silently deadlocks
// subsequent calls. Asserts via `inflight` returning to 0 after each
// failure.
func TestOllamaClient_SemaphoreReleasedOnNonCtxErrors(t *testing.T) {
	type scenario struct {
		name    string
		handler http.HandlerFunc
	}
	scenarios := []scenario{
		{
			name: "http_500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			},
		},
		{
			name: "decode_error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not-json"))
			},
		},
		{
			name: "empty_embeddings",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"embeddings":[]}`))
			},
		},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			var inflight, peak int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cur := atomic.AddInt64(&inflight, 1)
				for {
					old := atomic.LoadInt64(&peak)
					if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
						break
					}
				}
				sc.handler(w, r)
				atomic.AddInt64(&inflight, -1)
			}))
			t.Cleanup(srv.Close)

			// Cap=1, no retries (isolating the single-attempt release path).
			client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
				MaxConcurrent: 1,
				EmbedTimeout:  2 * time.Second,
				MaxRetries:    0,
			})

			// Burn the slot 3 times in succession. If the slot leaks on
			// the failing path, the 2nd or 3rd call would block on the
			// semaphore and exceed the ctx deadline.
			for i := 0; i < 3; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := client.Embed(ctx, "m", "t")
				cancel()
				if err == nil {
					t.Fatalf("iter %d: expected error", i)
				}
				// Should not be ctx timeout — that'd indicate the slot
				// leaked and we waited forever.
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("iter %d: DeadlineExceeded (likely semaphore leak): %v", i, err)
				}
			}
			// After all calls complete, inflight must have returned
			// to 0 and peak must never have exceeded 1.
			if got := atomic.LoadInt64(&inflight); got != 0 {
				t.Fatalf("inflight = %d, want 0 (semaphore leaked)", got)
			}
			if got := atomic.LoadInt64(&peak); got > 1 {
				t.Fatalf("peak = %d, want <= 1", got)
			}
		})
	}
}

// Note: EmbedBatch retry path is covered by the existing
// `TestOllamaClient_RetryOnDeadlineExceeded` for the single-text case.
// A dedicated batch retry test was evaluated but the `applyBatchDeadline`
// helper adds `len(texts)*2s` to the per-request budget, so a realistic
// multi-text retry scenario requires a ~12s wall-clock budget. Since
// the retry gate is shared structurally (isPerRequestDeadlineExceeded),
// this is review IM-2 (medium) deferred; the regression value is low
// and the test-runtime cost is high. See pr74-review-tests.md IM-2.

// TestOllamaClient_RetryBackoffGrows: backoff durations are consumed in
// order. Verified indirectly by measuring elapsed time of a retry chain.
func TestOllamaClient_RetryBackoffGrows(t *testing.T) {
	var attempts int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&attempts, 1)
		if n < 3 {
			select {
			case <-done:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[[1.0]]}`))
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	client := NewOllamaClientWithOptions(srv.URL, OllamaOptions{
		MaxConcurrent: 2,
		EmbedTimeout:  50 * time.Millisecond,
		MaxRetries:    2,
		RetryBackoff:  []time.Duration{80 * time.Millisecond, 160 * time.Millisecond},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.Embed(ctx, "m", "t")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Minimum elapsed ≈ 50ms (first attempt) + 80ms (backoff) + 50ms
	// (second attempt) + 160ms (backoff) + ~1ms (third) = ~340ms.
	if elapsed < 280*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 280ms (backoffs should have been applied)", elapsed)
	}
}
