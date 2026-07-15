package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a config pointing /ptc at the given upstream URL.
func writeConfig(t *testing.T, path, secret, upstreamURL string) {
	t.Helper()
	body := fmt.Sprintf(`
listen = ":9090"
secret = %q

[[pool]]
path = "/ptc"

  [[pool.upstream]]
  url = %q
  secret = "up-secret"
`, secret, upstreamURL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func postTo(h *hub, path, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"username":"u"}`))
	req.Header.Set(secretHeader, secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func echoServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"login_code":"`+name+`","status":"OK"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// Reload must repoint live traffic at the new upstream.
func TestReloadSwapsUpstreams(t *testing.T) {
	before, after := echoServer(t, "BEFORE"), echoServer(t, "AFTER")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "s", before.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}
	if body := postTo(h, "/ptc", "s").Body.String(); !strings.Contains(body, "BEFORE") {
		t.Fatalf("body = %q, want BEFORE", body)
	}

	writeConfig(t, path, "s", after.URL)
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}
	if body := postTo(h, "/ptc", "s").Body.String(); !strings.Contains(body, "AFTER") {
		t.Errorf("body = %q, want AFTER — reload did not swap the upstream", body)
	}
}

// A broken config must not take auth down. This is the whole reason reload
// builds everything before swapping anything.
func TestReloadKeepsPreviousConfigOnError(t *testing.T) {
	up := echoServer(t, "LIVE")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "s", up.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("this is not valid toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reload(path, newTransport()); err == nil {
		t.Fatal("reload accepted a broken config")
	}

	// Still serving on the old pools.
	if body := postTo(h, "/ptc", "s").Body.String(); !strings.Contains(body, "LIVE") {
		t.Errorf("body = %q — a broken config took the pool down", body)
	}
}

// A config that parses but is invalid must be rejected on reload too, not just
// at startup, and the pool it would have replaced must survive it.
func TestReloadRejectsInvalidConfig(t *testing.T) {
	up := echoServer(t, "LIVE")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "s", up.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	// Valid TOML, but nothing can dial it.
	writeConfig(t, path, "s", "ftp://nope.invalid/login-code")
	if _, err := h.reload(path, newTransport()); err == nil {
		t.Fatal("reload accepted an upstream with a non-http scheme")
	}
	if body := postTo(h, "/ptc", "s").Body.String(); !strings.Contains(body, "LIVE") {
		t.Errorf("body = %q — an invalid config took the pool down", body)
	}
}

// An empty secret is allowed on purpose: it warns loudly but still serves, so
// that a hub behind its own firewall isn't forced to invent one. It must not be
// rejected — reload and startup agree on that.
func TestReloadAllowsEmptySecret(t *testing.T) {
	up := echoServer(t, "LIVE")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "", up.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatalf("reload rejected an empty secret: %v", err)
	}
	if body := postTo(h, "/ptc", "").Body.String(); !strings.Contains(body, "LIVE") {
		t.Errorf("body = %q — an empty secret should still serve", body)
	}
}

// Rotating the inbound secret takes effect on reload.
func TestReloadSwapsInboundSecret(t *testing.T) {
	up := echoServer(t, "LIVE")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "old", up.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, path, "new", up.URL)
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	if code := postTo(h, "/ptc", "old").Code; code != http.StatusForbidden {
		t.Errorf("old secret got %d, want 403", code)
	}
	if code := postTo(h, "/ptc", "new").Code; code != 200 {
		t.Errorf("new secret got %d, want 200", code)
	}
}

// A pool dropped from the config stops being served.
func TestReloadRemovesPool(t *testing.T) {
	up := echoServer(t, "LIVE")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "s", up.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`
listen = ":9090"
secret = "s"

[[pool]]
path = "/google"

  [[pool.upstream]]
  url = %q
  secret = "up-secret"
`, up.URL)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	if code := postTo(h, "/ptc", "s").Code; code != http.StatusNotFound {
		t.Errorf("removed pool got %d, want 404", code)
	}
	if code := postTo(h, "/google", "s").Code; code != 200 {
		t.Errorf("added pool got %d, want 200", code)
	}
}

func TestRouterRejectsNonPost(t *testing.T) {
	up := echoServer(t, "LIVE")
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "s", up.URL)

	h := &hub{}
	if _, err := h.reload(path, newTransport()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/ptc", nil)
	req.Header.Set(secretHeader, "s")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET got %d, want 405", w.Code)
	}
}

// stamp has to notice a rewrite that keeps the same size, which is what an
// operator editing a secret in place looks like.
func TestStampNoticesSameSizeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := stamp(path)

	if err := os.WriteFile(path, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if second := stamp(path); second == first {
		t.Errorf("stamp unchanged (%q) after a same-size rewrite", second)
	}
}

func TestStampOnMissingFile(t *testing.T) {
	if s := stamp(filepath.Join(t.TempDir(), "gone.toml")); s != "" {
		t.Errorf("stamp = %q, want empty for a missing file", s)
	}
}

// writeTemp writes body to a temp config file and returns its path.
func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
