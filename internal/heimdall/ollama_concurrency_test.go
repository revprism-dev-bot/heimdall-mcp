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

// slowEmbedServer returns an httptest.Server whose /api/embed handler records
// in-flight concurrency (peak and current) and blocks until `release` is
// closed or ctx is done. Lets tests assert on semaphore bounding without
// time-based flakiness.
func slowEmbedServer(t *testing.T, release <-chan struct{}) (*httptest.Server, *int64, *int64) {
	t.Helper()
	var inflight, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		defer atomic.AddInt64(&inflight, -1)

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
	return srv, &inflight, &peak
}

// TestOllamaSemaphore_BoundsConcurrency: a client constructed with cap=2
// must never allow more than 2 in-flight /api/embed requests, even when
// 10 goroutines call Embed concurrently.
func TestOllamaSemaphore_BoundsConcurrency(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 2)

	const N = 10
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

	// Give goroutines time to queue up on the semaphore.
	time.Sleep(200 * time.Millisecond)

	// Now release the server. All requests will return.
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("embed: unexpected error %v", err)
		}
	}

	if got := atomic.LoadInt64(peak); got > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2", got)
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
	srv, _, peak := slowEmbedServer(t, release)

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
	time.Sleep(200 * time.Millisecond)
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
	srv, _, peak := slowEmbedServer(t, release)

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
	time.Sleep(200 * time.Millisecond)

	// With cap=0 (unbounded), peak should equal N eventually.
	if got := atomic.LoadInt64(peak); got < int64(N) {
		// Race: server may not have picked them all up yet. Retry briefly.
		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) && atomic.LoadInt64(peak) < int64(N) {
			time.Sleep(20 * time.Millisecond)
		}
	}
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
	srv, _, peak := slowEmbedServer(t, release)
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
	time.Sleep(200 * time.Millisecond)
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
	srv, _, _ := slowEmbedServer(t, release)
	defer close(release)

	client := NewOllamaClientWithLimit(srv.URL, 1)

	// Occupy the sole slot with a goroutine that will block forever until
	// `release` closes (via defer above).
	started := make(chan struct{})
	go func() {
		close(started)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.Embed(ctx, "m", "blocker")
	}()
	<-started
	time.Sleep(100 * time.Millisecond) // ensure blocker holds the slot

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
	srv, _, peak := slowEmbedServer(t, release)

	client := NewOllamaClientWithLimit(srv.URL, 3)

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
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(peak); got > 3 {
		t.Fatalf("peak in-flight = %d, want <= 3", got)
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
	srv, _, peak := slowEmbedServer(t, release)

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
	time.Sleep(200 * time.Millisecond)
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
	srv, _, peak := slowEmbedServer(t, release)

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
	time.Sleep(200 * time.Millisecond)
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
func TestOllamaClient_EmbedBatch_AlsoBounded(t *testing.T) {
	release := make(chan struct{})
	var inflight, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
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
	defer srv.Close()

	client := NewOllamaClientWithLimit(srv.URL, 2)

	const N = 8
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
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2 (batch must respect semaphore)", got)
	}
}

// TestNewOllamaClientFromConfig_AppliesConfig: verifies the config-flow
// helper wires MaxConcurrent, EmbedTimeout, and MaxRetries through.
func TestNewOllamaClientFromConfig_AppliesConfig(t *testing.T) {
	release := make(chan struct{})
	srv, _, peak := slowEmbedServer(t, release)

	// maxConcurrent=1, timeoutMs=0 (→ default), maxRetries=0 (→ default 2).
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
	time.Sleep(200 * time.Millisecond)
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

// TestNewOllamaClientFromConfig_NegativeRetriesCoercesToZero: a -1 sentinel
// means "explicitly disable retries" (config JSON zero is ambiguous).
func TestNewOllamaClientFromConfig_NegativeRetriesCoercesToZero(t *testing.T) {
	client := NewOllamaClientFromConfig("http://127.0.0.1:0", 2, 1000, -1)
	if client.maxRetries != 0 {
		t.Fatalf("maxRetries = %d, want 0 (negative sentinel → zero retries)", client.maxRetries)
	}
	if client.embedTimeout != time.Second {
		t.Fatalf("embedTimeout = %v, want 1s", client.embedTimeout)
	}
}

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
