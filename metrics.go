package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsPath is where metrics are served, on the same listener as the proxy.
// Reserved in config validation so a pool can never claim it.
const metricsPath = "/metrics"

// metrics holds the Prometheus collectors and the handler that renders them.
//
// One instance is built once at startup and registered once, because
// re-registering a collector panics and a reload must never. The reload only
// flips `enabled`: an atomic so the /metrics handler and the request path can
// read it without a lock, and so turning stats on or off is a live config
// change like everything else here, not a restart.
type metrics struct {
	enabled atomic.Bool
	handler http.Handler

	requests  *prometheus.CounterVec   // pool, upstream, status — one final outcome per request
	duration  *prometheus.HistogramVec // pool, upstream — per-try upstream latency
	failovers *prometheus.CounterVec   // pool, upstream — transport failure, tried the next one
	exhausted *prometheus.CounterVec   // pool — every upstream failed
	aborted   *prometheus.CounterVec   // pool — caller gave up before an answer
	inflight  *prometheus.GaugeVec     // pool — requests currently being proxied
}

// newMetrics builds the collectors on a registry of their own — not the global
// default — so a test can stand up an instance without the duplicate
// registration panic that a second use of the default registry would cause.
// The Go and process collectors ride along for the runtime-health row of the
// dashboard.
func newMetrics() *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authhub_auth_requests_total",
			Help: "Auth requests by final outcome, per pool and upstream.",
		}, []string{"pool", "upstream", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "authhub_auth_request_duration_seconds",
			Help:    "Upstream response latency per try, in seconds.",
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 45, 60},
		}, []string{"pool", "upstream"}),
		failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authhub_upstream_failovers_total",
			Help: "Transport failures that fell over to the next upstream, per pool and upstream.",
		}, []string{"pool", "upstream"}),
		exhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authhub_pool_exhausted_total",
			Help: "Requests where every upstream in the pool failed, per pool.",
		}, []string{"pool"}),
		aborted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "authhub_caller_aborted_total",
			Help: "Requests where the caller gave up before an upstream answered, per pool.",
		}, []string{"pool"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "authhub_requests_in_flight",
			Help: "Requests currently being proxied, per pool.",
		}, []string{"pool"}),
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		m.requests, m.duration, m.failovers, m.exhausted, m.aborted, m.inflight,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return m
}

// on reports whether stats should be recorded. Nil-safe so a pool built without
// metrics (every test that calls newPool directly) is a silent no-op.
func (m *metrics) on() bool { return m != nil && m.enabled.Load() }

// observeResponse records a try that produced an HTTP response: the upstream's
// self-reported status out of the body, and how long the try took. The body is
// read and put back byte-for-byte, so the proxied reply is exactly what the
// upstream sent — sniffing must never change what Dragonite sees.
func (m *metrics) observeResponse(pool, upstream string, dispatchedAt time.Time, resp *http.Response) {
	if !m.on() {
		return
	}
	status := sniffStatus(resp)
	m.requests.WithLabelValues(pool, upstream, status).Inc()
	m.duration.WithLabelValues(pool, upstream).Observe(time.Since(dispatchedAt).Seconds())
}

// observeFailover counts an upstream that failed at the transport level and was
// passed over for the next one. It's an intermediate event, not a request
// outcome, so it lives apart from requests_total.
func (m *metrics) observeFailover(pool, upstream string) {
	if !m.on() {
		return
	}
	m.failovers.WithLabelValues(pool, upstream).Inc()
}

// observeExhausted counts a request where every upstream failed. It records the
// final error outcome too, so requests_total stays the single tally of what
// every request ended as — the answering try, or this synthetic ERROR.
func (m *metrics) observeExhausted(pool, upstream string) {
	if !m.on() {
		return
	}
	m.exhausted.WithLabelValues(pool).Inc()
	m.requests.WithLabelValues(pool, upstream, "error").Inc()
}

// observeAborted counts a request the caller abandoned before any upstream
// answered. It is deliberately not a requests_total outcome: nothing was
// delivered, and the caller left of its own accord rather than an upstream
// failing it.
func (m *metrics) observeAborted(pool string) {
	if !m.on() {
		return
	}
	m.aborted.WithLabelValues(pool).Inc()
}

// inflightAdd moves the in-flight gauge. It guards only on a nil metrics, not on
// enabled: skipping the decrement of a request that began while enabled would
// leave the gauge stuck above zero, so it's kept balanced regardless. When
// stats are off the gauge simply isn't scraped.
func (m *metrics) inflightAdd(pool string, delta float64) {
	if m == nil {
		return
	}
	m.inflight.WithLabelValues(pool).Add(delta)
}

// serveMetrics answers /metrics, or 404s when stats are disabled so the path
// isn't a tell that the feature exists.
func (m *metrics) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if !m.on() {
		http.NotFound(w, r)
		return
	}
	m.handler.ServeHTTP(w, r)
}

// sniffStatus reads the upstream's `status` field out of the response body and
// restores the body untouched. Auth replies are a few hundred bytes; the
// LimitReader is only there so a runaway body can't be pulled whole into memory,
// and if one ever is, the read prefix is stitched back in front of the rest so
// the caller still gets the complete response — just unparsed.
func sniffStatus(resp *http.Response) string {
	body := resp.Body
	if body == nil {
		return "unknown"
	}

	buf, err := io.ReadAll(io.LimitReader(body, maxBodyBytes+1))
	if len(buf) > maxBodyBytes {
		resp.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(buf), body), Closer: body}
		return "unknown"
	}
	// Fully buffered — hand the caller an identical copy and close the original,
	// since NopCloser won't.
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	_ = body.Close()
	if err != nil {
		return "unknown"
	}

	var parsed struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(buf, &parsed) != nil {
		return "unknown"
	}
	return normalizeStatus(parsed.Status)
}

// readCloser splices a reader onto an unrelated Close, for the oversize body
// path where the bytes come from a MultiReader but the original body still owns
// the connection.
type readCloser struct {
	io.Reader
	io.Closer
}

// normalizeStatus maps the upstream's status onto a small fixed set of label
// values. Anything unrecognised becomes "unknown" rather than passing through:
// a status label built from arbitrary upstream text is an unbounded number of
// Prometheus series waiting for one upstream to misbehave.
func normalizeStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SUCCESS", "OK":
		return "success"
	case "INVALID":
		return "invalid"
	case "BANNED":
		return "banned"
	case "TIMEOUT":
		return "timeout"
	case "ERROR":
		return "error"
	default:
		return "unknown"
	}
}
