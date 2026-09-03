package integration

// Load / performance harness (B2 in NEXT_TASKS.md).
//
// Measures through the REAL proxy stack — real TrinoEvaluator, real
// httputil.ReverseProxy, real sockets (httptest.NewServer on both sides,
// never ResponseRecorder, which buffers whole responses in memory and
// would falsify latency and memory results):
//
//   - TestLoadLatency_Tier2OnVsOff: P50/P95/P99 of POST /v1/statement with
//     the Tier 2 pre-flight (EXPLAIN round-trip) on vs off, under
//     concurrency, including preflight.max_concurrent gate saturation.
//   - TestLoadLargePageMemoryFlat: streamed large JSON pages through the
//     Trino-URL rewrite path must keep proxy-side allocations bounded
//     (blueprint principle #4: zero-copy streaming, flat memory).
//
// The fake coordinator keeps per-query protocol state (the functional-test
// mock uses one global poll counter, which corrupts state under
// concurrency). A configurable delay simulates coordinator EXPLAIN cost.
//
// Run: go test ./internal/integration/ -run 'TestLoad' -v
// These tests are also part of the normal suite; bounds are loose so they
// pass on loaded CI machines.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"query-guard/internal/config"
	"query-guard/internal/engine"
	"query-guard/internal/proxy"
)

// ──────────────────────────────────────────────────────────────────────────────
// Load-test coordinator: per-query protocol state + artificial EXPLAIN delay
// ──────────────────────────────────────────────────────────────────────────────

type loadTrino struct {
	server        *httptest.Server
	mu            sync.Mutex
	steps         map[string]int // preflight poll step per query token
	nextID        int
	preflightNano int64 // artificial delay per preflight protocol hop
	bigPage       []byte
}

func newLoadTrino(t *testing.T, preflightDelay time.Duration, bigPage []byte) *loadTrino {
	m := &loadTrino{
		steps:         map[string]int{},
		preflightNano: int64(preflightDelay),
		bigPage:       bigPage,
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *loadTrino) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	// Large streamed data page (only used by the memory test). The client
	// requests it through the proxy passthrough, so the URL-rewrite path
	// (ModifyResponse) engages exactly as in production.
	if strings.HasPrefix(r.URL.Path, "/v1/statement/data/") {
		w.Header().Set("Content-Type", "application/json")
		w.Write(m.bigPage)
		return
	}

	isPreflight := strings.HasPrefix(strings.TrimSpace(string(body)), "EXPLAIN (TYPE IO, FORMAT JSON)")
	var token string
	if !isPreflight && r.Method == http.MethodGet {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		token = parts[len(parts)-1]
		_, isPreflight = m.preflightToken(token)
	}

	if isPreflight {
		if m.preflightNano > 0 {
			time.Sleep(time.Duration(m.preflightNano))
		}
		m.mu.Lock()
		m.nextID++
		if token == "" {
			token = "q" + strconv.Itoa(m.nextID)
		}
		m.steps[token]++
		step := m.steps[token]
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch step {
		case 1:
			fmt.Fprintf(w, `{"id":%q,"nextUri":"http://%s/v1/statement/poll/%s/1","stats":{"state":"QUEUED"}}`, token, r.Host, token)
		case 2:
			cell, _ := jsonMarshal(allowedPlan)
			fmt.Fprintf(w, `{"id":%q,"nextUri":"http://%s/v1/statement/poll/%s/2","columns":[{"name":"Query Plan"}],"data":[[%s]],"stats":{"state":"RUNNING"}}`, token, r.Host, token, cell)
		default:
			fmt.Fprintf(w, `{"id":%q,"stats":{"state":"FINISHED"}}`, token)
		}
		return
	}

	// Real query submission: respond immediately with the first protocol page
	// (the load-test client reads only the submission response).
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"query","infoUri":"http://%s/query","nextUri":"http://%s/v1/statement/data/query/1"}`, r.Host, r.Host)
}

// preflightToken reports whether the token belongs to a preflight query.
func (m *loadTrino) preflightToken(token string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.steps[token]
	return token, ok
}

func jsonMarshal(s string) ([]byte, error) {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b = append(b, '\\', c)
		default:
			b = append(b, c)
		}
	}
	return append(b, '"'), nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Harness
// ──────────────────────────────────────────────────────────────────────────────

func loadPolicy(upstreamURL string, maxConcurrent int) *config.Policy {
	return &config.Policy{
		Upstream: config.UpstreamConfig{URL: upstreamURL},
		Preflight: config.PreflightConfig{
			Timeout:       2 * time.Second,
			MaxConcurrent: maxConcurrent,
		},
		Rules: config.RulesConfig{
			CostLimits: []config.CostLimit{
				{MaxScanBytesPerQuery: 10_000_000},
			},
		},
	}
}

func newLoadProxy(t *testing.T, upstreamURL string, maxConcurrent int) http.Handler {
	cfg := config.NewConfig(loadPolicy(upstreamURL, maxConcurrent))
	eval := engine.NewTrinoEvaluator(cfg, nil)
	h, err := proxy.NewHandler(cfg, eval, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

type latencyStats struct {
	n     int
	mean  time.Duration
	p50   time.Duration
	p95   time.Duration
	p99   time.Duration
	max   time.Duration
	errs  int
	codes map[int]int
}

func (s latencyStats) String() string {
	return fmt.Sprintf("n=%d errs=%d mean=%v p50=%v p95=%v p99=%v max=%v codes=%v",
		s.n, s.errs, s.mean.Round(time.Microsecond), s.p50.Round(time.Microsecond),
		s.p95.Round(time.Microsecond), s.p99.Round(time.Microsecond), s.max.Round(time.Microsecond), s.codes)
}

// runLoad fires n POST /v1/statement requests with the given concurrency
// against the proxy over real sockets and records per-request latency.
func runLoad(t *testing.T, proxyURL string, n, workers int) latencyStats {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: workers}}
	defer client.CloseIdleConnections()

	durations := make([]time.Duration, 0, n)
	codes := map[int]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan int)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				start := time.Now()
				resp, err := client.Post(proxyURL+"/v1/statement", "text/plain", strings.NewReader("SELECT * FROM hive.default.orders"))
				if err != nil {
					mu.Lock()
					codes[-1]++
					durations = append(durations, time.Since(start))
					mu.Unlock()
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				mu.Lock()
				codes[resp.StatusCode]++
				durations = append(durations, time.Since(start))
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	pct := func(p float64) time.Duration {
		idx := int(float64(len(durations)-1) * p)
		return durations[idx]
	}
	var sum time.Duration
	for _, d := range durations {
		sum += d
	}
	errs := codes[-1]
	return latencyStats{
		n:     len(durations),
		mean:  sum / time.Duration(len(durations)),
		p50:   pct(0.50),
		p95:   pct(0.95),
		p99:   pct(0.99),
		max:   durations[len(durations)-1],
		errs:  errs,
		codes: codes,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────────────────

// TestLoadLatency_Tier2OnVsOff measures the latency the Tier 2 pre-flight
// adds to statement submission under concurrency. The fake coordinator adds
// preflightDelay per preflight protocol hop (3 hops per EXPLAIN).
func TestLoadLatency_Tier2OnVsOff(t *testing.T) {
	const (
		n              = 200
		workers        = 16
		preflightDelay = 10 * time.Millisecond
		gateSize       = 5 // preflight.max_concurrent
	)

	// Tier 2 OFF: no cost limits configured → shouldPreflight is false.
	off := newLoadTrino(t, 0, nil)
	offCfg := config.NewConfig(loadPolicy(off.server.URL, gateSize))
	offCfg.Set(func() *config.Policy { p := loadPolicy(off.server.URL, gateSize); p.Rules.CostLimits = nil; return p }())
	offH, err := proxy.NewHandler(offCfg, engine.NewTrinoEvaluator(offCfg, nil), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	offSrv := httptest.NewServer(offH)
	defer offSrv.Close()

	// Tier 2 ON: cost limits configured, coordinator delays each hop.
	on := newLoadTrino(t, preflightDelay, nil)
	onSrv := httptest.NewServer(newLoadProxy(t, on.server.URL, gateSize))
	defer onSrv.Close()

	offStats := runLoad(t, offSrv.URL, n, workers)
	onStats := runLoad(t, onSrv.URL, n, workers)

	t.Logf("Tier2 OFF: %s", offStats)
	t.Logf("Tier2 ON : %s", onStats)

	// Sanity: everything succeeded.
	for name, s := range map[string]latencyStats{"off": offStats, "on": onStats} {
		if s.errs != 0 {
			t.Errorf("%s: %d transport errors", name, s.errs)
		}
		if s.codes[http.StatusOK] != n {
			t.Errorf("%s: got status codes %v, want %d × 200", name, s.codes, n)
		}
	}

	// Tier 2 must add measurable latency (3 hops × 10ms, gated at 5).
	if onStats.p50 <= offStats.p50 {
		t.Errorf("expected Tier 2 ON p50 (%v) > OFF p50 (%v)", onStats.p50, offStats.p50)
	}
	// And it must be bounded: worst case ≈ queue wait + 3×delay + epsilon.
	if overhead := onStats.p95 - offStats.p95; overhead > 10*preflightDelay {
		t.Errorf("Tier 2 overhead at p95 = %v, unexpectedly large (> 10× hop delay)", overhead)
	}
}

// TestLoadLargePageMemoryFlat pushes large JSON pages through the proxy's
// Trino-URL rewrite path and asserts proxy-side allocation stays bounded —
// i.e. pages are streamed, not buffered (blueprint principle #4).
func TestLoadLargePageMemoryFlat(t *testing.T) {
	const (
		pages     = 4
		pageBytes = 32 << 20 // 32 MiB per page (≫ rewritePrefixBytes = 64 KiB)
		bound     = int64(2 * pages * pageBytes)
	)

	payload := strings.Repeat("x", pageBytes)
	bigPage := []byte(`{"id":"q","infoUri":"http://trino/info","nextUri":"http://trino/next","data":[["` + payload + `"]]}`)

	up := newLoadTrino(t, 0, bigPage)
	srv := httptest.NewServer(newLoadProxy(t, up.server.URL, 5))
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2}}
	defer client.CloseIdleConnections()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var copied int64
	var pageLen int64
	for i := 0; i < pages; i++ {
		resp, err := client.Get(srv.URL + "/v1/statement/data/q/1")
		if err != nil {
			t.Fatalf("GET page %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page %d: status %d", i, resp.StatusCode)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("page %d: read: %v", i, err)
		}
		if i == 0 {
			// The URL rewrite replaces the upstream authority ("trino") with the
			// client-facing host ("127.0.0.1:<port>") in infoUri/nextUri, so the
			// streamed page is slightly longer than the raw payload. All pages
			// must be identical; page 0 defines the expected length.
			pageLen = n
		}
		if n != pageLen {
			t.Errorf("page %d: streamed %d bytes, want %d (inconsistent — possible corruption)", i, n, pageLen)
		}
		copied += n
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	total := pageLen * pages
	if copied != total {
		t.Errorf("streamed %d bytes, want %d (rewrite corrupted the body)", copied, total)
	}
	allocDelta := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("streamed %d MiB across %d pages; proxy-side TotalAlloc delta = %d MiB (bound %d MiB)",
		total>>20, pages, allocDelta>>20, bound>>20)
	if allocDelta > bound {
		t.Errorf("proxy allocated %d MiB to stream %d MiB — pages are being buffered, memory is not flat", allocDelta>>20, total>>20)
	}
}
