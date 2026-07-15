package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A dead upstream must not look like a credential problem: Dragonite reacts to
// INVALID/BANNED by permanently marking the account.
func TestUpstreamDownNeverReportsInvalidOrBanned(t *testing.T) {
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
