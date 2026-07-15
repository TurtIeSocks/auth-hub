package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestPool wires a pool at /ptc against the given upstream configs.
func newTestPool(t *testing.T, ups ...upstreamConfig) *pool {
	t.Helper()
	p, err := newPool(poolConfig{Path: "/ptc", Upstreams: ups}, "dnite-secret", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func post(p *pool, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/ptc", strings.NewReader(`{"url":"https://access.pokemon.com/oauth2/auth","username":"u","password":"p","proxy":""}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(secretHeader, secret)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	return w
}

// The core of the whole service: requests spread across upstreams, each one
// getting its own secret, and Dragonite's secret never leaking through.
func TestRoundRobinSwapsSecretPerUpstream(t *testing.T) {
	var got []string
	srv := func(name, wantSecret string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s := r.Header.Get(secretHeader); s != wantSecret {
				t.Errorf("%s: secret = %q, want %q", name, s, wantSecret)
			}
			if r.URL.Path != "/login-code" {
				t.Errorf("%s: path = %q, want /login-code", name, r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"username":"u"`) {
				t.Errorf("%s: body not forwarded: %s", name, body)
			}
			got = append(got, name)
			_, _ = io.WriteString(w, `{"login_code":"code-`+name+`","status":"OK"}`)
		}))
	}
	a := srv("a", "secret-a")
	defer a.Close()
	b := srv("b", "secret-b")
	defer b.Close()

	p := newTestPool(t,
		upstreamConfig{Url: a.URL + "/login-code", Secret: "secret-a"},
		upstreamConfig{Url: b.URL + "/login-code", Secret: "secret-b"},
	)

	for i := 0; i < 4; i++ {
		if w := post(p, "dnite-secret"); w.Code != 200 {
			t.Fatalf("request %d: code = %d", i, w.Code)
		}
	}

	if want := []string{"a", "b", "a", "b"}; !equal(got, want) {
		t.Errorf("hit order = %v, want %v", got, want)
	}
}

func TestWrongInboundSecretRejected(t *testing.T) {
	hit := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer up.Close()

	p := newTestPool(t, upstreamConfig{Url: up.URL, Secret: "s"})

	if w := post(p, "wrong"); w.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", w.Code)
	}
	if hit {
		t.Error("upstream was reached with a bad secret")
	}
}

// A dead upstream should be invisible to Dragonite as long as a live one is
// left: the request fails over rather than failing.
func TestFailoverToNextUpstream(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := r.Header.Get(secretHeader); s != "live-secret" {
			t.Errorf("retry sent secret %q, want live-secret", s)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"username":"u"`) {
			t.Errorf("retry lost the body: %s", body)
		}
		_, _ = io.WriteString(w, `{"login_code":"CODE","status":"OK"}`)
	}))
	defer live.Close()

	p := newTestPool(t,
		upstreamConfig{Url: deadURL, Secret: "dead-secret"},
		upstreamConfig{Url: live.URL, Secret: "live-secret"},
	)

	// start=0 hits the dead one first and must fail over to the live one.
	w := post(p, "dnite-secret")
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200 (failover did not happen)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"login_code":"CODE"`) {
		t.Errorf("body = %q, want the live upstream's answer", w.Body)
	}
}

// Every upstream must be tried before giving up, and no upstream twice.
func TestFailoverTriesEachUpstreamOnce(t *testing.T) {
	// The handlers never answer, so there is no response to synchronise the
	// test goroutine with theirs. Guard the record explicitly.
	var mu sync.Mutex
	var hits []string
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, name)
			mu.Unlock()
			// Hijack and close without answering: a transport-level failure.
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			c.Close()
		}))
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	defer a.Close()
	defer b.Close()
	defer c.Close()

	p := newTestPool(t,
		upstreamConfig{Url: a.URL, Secret: "s"},
		upstreamConfig{Url: b.URL, Secret: "s"},
		upstreamConfig{Url: c.URL, Secret: "s"},
	)

	post(p, "dnite-secret")

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"a", "b", "c"}; !equal(hits, want) {
		t.Errorf("tried %v, want %v (each upstream exactly once)", hits, want)
	}
}

// With every upstream dead there is nothing left to try, and the answer must
// still not look like a credential problem: Dragonite reacts to INVALID/BANNED
// by permanently marking the account.
func TestAllUpstreamsDownNeverReportsInvalidOrBanned(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening now

	p := newTestPool(t, upstreamConfig{Url: deadURL, Secret: "s"})
	w := post(p, "dnite-secret")

	var resp struct {
		LoginCode string `json:"login_code"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body must stay JSON for Dragonite to parse, got %q: %v", w.Body, err)
	}
	if resp.Status == "INVALID" || resp.Status == "BANNED" {
		t.Fatalf("status = %q — this would permanently mark the account", resp.Status)
	}
	if resp.LoginCode != "" {
		t.Errorf("login_code = %q, want empty", resp.LoginCode)
	}
}

// Upstream verdicts are the upstream's to make; pass them through untouched.
func TestUpstreamStatusPassesThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"login_code":"","status":"INVALID"}`)
	}))
	defer up.Close()

	p := newTestPool(t, upstreamConfig{Url: up.URL, Secret: "s"})
	w := post(p, "dnite-secret")

	if body := w.Body.String(); !strings.Contains(body, `"status":"INVALID"`) {
		t.Errorf("body = %q, want INVALID passed through", body)
	}
}

func TestSingleUpstreamPoolWorks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"login_code":"x","status":"OK"}`)
	}))
	defer up.Close()

	p := newTestPool(t, upstreamConfig{Url: up.URL, Secret: "s"})
	for i := 0; i < 3; i++ {
		if w := post(p, "dnite-secret"); w.Code != 200 {
			t.Fatalf("code = %d", w.Code)
		}
	}
}

// An upstream with no secret configured must not inherit Dragonite's.
func TestEmptyUpstreamSecretStripsHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header[secretHeader]; ok {
			t.Errorf("secret header should be absent, got %q", r.Header.Get(secretHeader))
		}
		_, _ = io.WriteString(w, `{"login_code":"x","status":"OK"}`)
	}))
	defer up.Close()

	p := newTestPool(t, upstreamConfig{Url: up.URL})
	post(p, "dnite-secret")
}

// countingRT fails every try and counts them, so a test can assert exactly how
// many upstreams a request burned.
type countingRT struct{ n atomic.Int32 }

func (c *countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("boom")
}

func newCountingPool(t *testing.T, rt http.RoundTripper) *pool {
	t.Helper()
	p, err := newPool(poolConfig{Path: "/ptc", Upstreams: []upstreamConfig{
		{Url: "http://a.invalid", Secret: "s"},
		{Url: "http://b.invalid", Secret: "s"},
		{Url: "http://c.invalid", Secret: "s"},
	}}, "dnite-secret", rt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func postCtx(p *pool, ctx context.Context) {
	req := httptest.NewRequest("POST", "/ptc", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set(secretHeader, "dnite-secret")
	p.ServeHTTP(httptest.NewRecorder(), req)
}

// A caller that has hung up, or spent its remote_auth_timeout_seconds, must not
// have the rest of the pool burned on its behalf — it isn't listening anymore.
func TestNoRetryAfterCallerGivesUp(t *testing.T) {
	rt := &countingRT{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	postCtx(newCountingPool(t, rt), ctx)

	if got := rt.n.Load(); got != 1 {
		t.Errorf("tried %d upstreams, want 1: a caller that gave up must not trigger failover", got)
	}
}

// The mirror of the above: a live caller does get every upstream tried.
func TestRetriesEveryUpstreamForLiveCaller(t *testing.T) {
	rt := &countingRT{}

	postCtx(newCountingPool(t, rt), context.Background())

	if got := rt.n.Load(); got != 3 {
		t.Errorf("tried %d upstreams, want 3", got)
	}
}

// The request body carries plaintext account credentials, so the transport must
// not pick up a proxy from the environment the way a cloned
// http.DefaultTransport would. Dragonite calling remote auth direct never did.
func TestTransportIgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://someone-elses-proxy:8080")

	req, err := http.NewRequest("POST", "http://auth-1:5090/api/v1/login-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := newTransport(); tr.Proxy != nil {
		u, err := tr.Proxy(req)
		if err != nil {
			t.Fatal(err)
		}
		if u != nil {
			t.Errorf("transport would send credentials via %v", u)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
