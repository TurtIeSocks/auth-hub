package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newMeteredPool is newTestPool with metrics wired and switched on, so a test
// can assert what a real reload-configured pool would record.
func newMeteredPool(t *testing.T, ups ...upstreamConfig) *pool {
	t.Helper()
	p := newTestPool(t, ups...)
	p.m = newMetrics()
	p.m.enabled.Store(true)
	return p
}

func TestNormalizeStatus(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"SUCCESS", "success"},
		{"OK", "success"}, // Dragonite's older wire word, and the test fixtures'
		{"ok", "success"},
		{"INVALID", "invalid"},
		{"BANNED", "banned"},
		{"TIMEOUT", "timeout"},
		{"ERROR", "error"},
		{" error ", "error"},
		{"", "unknown"},
		{"WAT", "unknown"}, // never a raw upstream string, or the label set is unbounded
		{"12345", "unknown"},
	} {
		if got := normalizeStatus(tc.in); got != tc.want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// sniffStatus must read the status without changing a single byte the caller
// sees — the whole point of a straight proxy.
func TestSniffStatusReadsAndRestoresBody(t *testing.T) {
	body := `{"login_code":"abc-123","status":"INVALID"}`
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	if got := sniffStatus(resp); got != "invalid" {
		t.Errorf("status = %q, want invalid", got)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body after sniff = %q, want %q (bytes must survive untouched)", got, body)
	}
}

// A body bigger than any real auth reply is left unparsed, but must still reach
// the caller whole rather than truncated at the read cap.
func TestSniffStatusPreservesOversizeBody(t *testing.T) {
	body := strings.Repeat("x", maxBodyBytes+500)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	if got := sniffStatus(resp); got != "unknown" {
		t.Errorf("status = %q, want unknown for an oversize body", got)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(body) {
		t.Errorf("body after sniff = %d bytes, want %d (oversize body was truncated)", len(got), len(body))
	}
}

// The headline: an upstream's status is counted, and the caller's response is
// byte-for-byte what the upstream sent.
func TestResponseStatusIsCountedAndPassedThrough(t *testing.T) {
	const reply = `{"login_code":"","status":"INVALID"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, reply)
	}))
	defer up.Close()

	p := newMeteredPool(t, upstreamConfig{Url: up.URL, Secret: "s", Label: "up-a"})
	w := post(p, "dnite-secret")

	if w.Body.String() != reply {
		t.Errorf("body = %q, want %q passed through untouched", w.Body.String(), reply)
	}
	if got := testutil.ToFloat64(p.m.requests.WithLabelValues("/ptc", "up-a", "invalid")); got != 1 {
		t.Errorf("requests_total{invalid} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(p.m.duration); got == 0 {
		t.Error("duration histogram recorded nothing")
	}
}

// Omitted label falls back to the URL host so a series always has a value.
func TestLabelFallsBackToHost(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"login_code":"x","status":"SUCCESS"}`)
	}))
	defer up.Close()

	p := newMeteredPool(t, upstreamConfig{Url: up.URL, Secret: "s"}) // no label
	post(p, "dnite-secret")

	host := p.upstreams[0].url.Host
	if p.upstreams[0].label != host {
		t.Fatalf("label = %q, want the host %q", p.upstreams[0].label, host)
	}
	if got := testutil.ToFloat64(p.m.requests.WithLabelValues("/ptc", host, "success")); got != 1 {
		t.Errorf("requests_total labelled by host = %v, want 1", got)
	}
}

// Off means off: no counting, and — just as important — the response body is
// never even read, let alone changed.
func TestDisabledMetricsRecordNothing(t *testing.T) {
	const reply = `{"login_code":"x","status":"SUCCESS"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, reply)
	}))
	defer up.Close()

	p := newTestPool(t, upstreamConfig{Url: up.URL, Secret: "s", Label: "up-a"})
	p.m = newMetrics() // wired but left disabled

	w := post(p, "dnite-secret")
	if w.Body.String() != reply {
		t.Errorf("body = %q, want %q", w.Body.String(), reply)
	}
	if got := testutil.CollectAndCount(p.m.requests); got != 0 {
		t.Errorf("requests_total has %d series while disabled, want 0", got)
	}
}

// Every upstream failing must count the failovers, the exhaustion, and one
// final error outcome — and must not misfire when metrics are off.
func TestFailoverAndExhaustionAreCounted(t *testing.T) {
	rt := &countingRT{} // fails every try
	p, err := newPool(poolConfig{Path: "/ptc", Upstreams: []upstreamConfig{
		{Url: "http://a.invalid", Secret: "s", Label: "a"},
		{Url: "http://b.invalid", Secret: "s", Label: "b"},
		{Url: "http://c.invalid", Secret: "s", Label: "c"},
	}}, "dnite-secret", rt)
	if err != nil {
		t.Fatal(err)
	}
	p.m = newMetrics()
	p.m.enabled.Store(true)

	post(p, "dnite-secret")

	// Three upstreams: two failovers (a->b, b->c) then exhaustion on c.
	if got := testutil.CollectAndCount(p.m.failovers); got != 2 {
		t.Errorf("failover series = %d, want 2", got)
	}
	if got := testutil.ToFloat64(p.m.exhausted.WithLabelValues("/ptc")); got != 1 {
		t.Errorf("pool_exhausted_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(p.m.requests.WithLabelValues("/ptc", "c", "error")); got != 1 {
		t.Errorf("requests_total{error} = %v, want 1", got)
	}
}

// The in-flight gauge has to come back down: an unbalanced add would leave it
// pinned above zero for ever.
func TestInflightReturnsToZero(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"login_code":"x","status":"SUCCESS"}`)
	}))
	defer up.Close()

	p := newMeteredPool(t, upstreamConfig{Url: up.URL, Secret: "s", Label: "up-a"})
	post(p, "dnite-secret")

	if got := testutil.ToFloat64(p.m.inflight.WithLabelValues("/ptc")); got != 0 {
		t.Errorf("requests_in_flight = %v after the request finished, want 0", got)
	}
}

// The endpoint serves when on and hides when off, so the path isn't a tell.
func TestMetricsEndpoint(t *testing.T) {
	h := &hub{metrics: newMetrics()}
	h.pools.Store(&map[string]*pool{})

	req := httptest.NewRequest("GET", metricsPath, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("disabled: code = %d, want 404", w.Code)
	}

	h.metrics.enabled.Store(true)
	// A family with no series is omitted from the output, so record one sample
	// before scraping — otherwise the assertion would fail on an empty vec, not
	// on the endpoint being wrong.
	h.metrics.requests.WithLabelValues("/ptc", "up-a", "success").Inc()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("enabled: code = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "authhub_auth_requests_total") {
		t.Error("metrics output is missing the auth-hub metrics")
	}
}

func TestConfigMetricsAndLabel(t *testing.T) {
	body := `listen = ":1"
secret = "s"

[metrics]
enabled = true

[[pool]]
path = "/ptc"

  [[pool.upstream]]
  url = "http://a:1/x"
  label = "auth-tokyo"
`
	cfg, err := loadConfig(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.Enabled {
		t.Error("metrics.enabled did not parse as true")
	}
	if cfg.Pools[0].Upstreams[0].Label != "auth-tokyo" {
		t.Errorf("label = %q, want auth-tokyo", cfg.Pools[0].Upstreams[0].Label)
	}
}

// Metrics default off, so a config with no [metrics] section exposes nothing.
func TestMetricsDefaultOff(t *testing.T) {
	cfg, err := loadConfig("config.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg // example may enable it; the zero value is what matters here:
	var empty metricsConfig
	if empty.Enabled {
		t.Error("metricsConfig zero value is enabled; it must default off")
	}
}

// /metrics is reserved whether or not stats are on, so enabling them later can't
// collide with a pool.
func TestPoolPathMetricsRejected(t *testing.T) {
	body := `listen = ":1"
secret = "s"

[[pool]]
path = "/metrics"

  [[pool.upstream]]
  url = "http://a:1/x"
`
	if _, err := loadConfig(writeTemp(t, body)); err == nil {
		t.Error("a pool at /metrics was accepted; it must be rejected")
	}
}
