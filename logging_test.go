package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLogger builds a logger split across two buffers, the way setupLogging
// splits one across stdout and stderr.
func testLogger(level slog.Level) (*slog.Logger, *bytes.Buffer, *bytes.Buffer) {
	var out, err bytes.Buffer
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: nameTrace}
	return slog.New(splitHandler{
		out: slog.NewTextHandler(&out, opts),
		err: slog.NewTextHandler(&err, opts),
	}), &out, &err
}

// The whole point of the split: stdout carries what happened, stderr carries
// only what went wrong.
func TestSplitHandlerRoutesByLevel(t *testing.T) {
	log, out, errBuf := testLogger(levelTrace)

	trace := func(msg string) { log.Log(context.Background(), levelTrace, msg) }
	trace("to-out-trace")
	log.Debug("to-out-debug")
	log.Info("to-out-info")
	log.Warn("to-err-warn")
	log.Error("to-err-error")

	for _, want := range []string{"to-out-trace", "to-out-debug", "to-out-info"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q, got: %s", want, out)
		}
		if strings.Contains(errBuf.String(), want) {
			t.Errorf("%q leaked to stderr", want)
		}
	}
	for _, want := range []string{"to-err-warn", "to-err-error"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("stderr missing %q, got: %s", want, errBuf)
		}
		if strings.Contains(out.String(), want) {
			t.Errorf("%q leaked to stdout", want)
		}
	}
}

// slog would render our level as "DEBUG-4" left to itself.
func TestTraceLevelIsNamed(t *testing.T) {
	log, out, _ := testLogger(levelTrace)
	log.Log(context.Background(), levelTrace, "hello")

	if !strings.Contains(out.String(), "level=TRACE") {
		t.Errorf("want level=TRACE, got: %s", out)
	}
}

// Below the configured level, nothing is written at all — including to the file
// the writers may be teed into.
func TestLevelFilters(t *testing.T) {
	log, out, _ := testLogger(slog.LevelInfo)
	log.Log(context.Background(), levelTrace, "trace-msg")
	log.Debug("debug-msg")
	log.Info("info-msg")

	if strings.Contains(out.String(), "trace-msg") || strings.Contains(out.String(), "debug-msg") {
		t.Errorf("level=info still logged below info: %s", out)
	}
	if !strings.Contains(out.String(), "info-msg") {
		t.Errorf("level=info dropped info: %s", out)
	}
}

// A record doesn't know which half it lands on until it has a level, so both
// halves have to carry whatever the logger was built up with.
func TestSplitHandlerKeepsAttrsOnBothSides(t *testing.T) {
	log, out, errBuf := testLogger(slog.LevelInfo)
	log = log.With("pool", "/ptc").WithGroup("g")

	log.Info("up")
	log.Error("down")

	if !strings.Contains(out.String(), "pool=/ptc") {
		t.Errorf("stdout lost the attr: %s", out)
	}
	if !strings.Contains(errBuf.String(), "pool=/ptc") {
		t.Errorf("stderr lost the attr: %s", errBuf)
	}
}

// A caller that hangs up is not an upstream's fault, and must not be reported
// as one at the level people point alerts at.
func TestCallerGaveUpIsNotAnUpstreamError(t *testing.T) {
	log, out, errBuf := testLogger(slog.LevelInfo)
	old := slog.Default()
	slog.SetDefault(log)
	t.Cleanup(func() { slog.SetDefault(old) })

	up := echoServer(t, "LIVE")
	p, err := newPool(poolConfig{
		Path:      "/ptc",
		Upstreams: []upstreamConfig{{Url: up.URL}},
	}, "s", newTransport())
	if err != nil {
		t.Fatal(err)
	}

	// A request whose caller has already gone away.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/ptc", strings.NewReader("{}")).WithContext(ctx)
	a := &attempt{in: req, body: []byte("{}")}
	req = req.WithContext(context.WithValue(ctx, attemptKey{}, a))

	p.errorHandler(httptest.NewRecorder(), req, context.Canceled)

	if strings.Contains(errBuf.String(), "every upstream failed") {
		t.Errorf("a caller hangup was blamed on the upstream:\n%s", errBuf)
	}
	if !strings.Contains(errBuf.String(), "caller gave up") {
		t.Errorf("want a caller-gave-up warning, got:\n%s%s", out, errBuf)
	}
	if strings.Contains(errBuf.String(), "level=ERROR") {
		t.Errorf("a caller hangup logged at ERROR:\n%s", errBuf)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"trace": levelTrace,
		"DEBUG": slog.LevelDebug, // case is not the user's problem
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := parseLevel(in)
		if err != nil || got != want {
			t.Errorf("parseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseLevel("verbose"); err == nil {
		t.Error("parseLevel accepted a level that doesn't exist")
	}
}

// writeLogConfig writes a minimal valid config with the given [log] body.
func writeLogConfig(t *testing.T, logSection string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `secret = "s"
` + logSection + `
[[pool]]
path = "/ptc"
  [[pool.upstream]]
  url = "http://up.invalid/login-code"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A config with no [log] section at all logs the way it always did.
func TestLogConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeLogConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("defaults = %q/%q, want info/text", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Log.File != "" {
		t.Errorf("file = %q, want the streams alone by default", cfg.Log.File)
	}
}

// Validated with everything else, so that a typo in it is caught by the same
// reload that keeps the last good config, rather than at the first log call.
func TestLogConfigRejectsGarbage(t *testing.T) {
	for _, section := range []string{
		"[log]\nlevel = \"verbose\"\n",
		"[log]\nformat = \"xml\"\n",
	} {
		if _, err := loadConfig(writeLogConfig(t, section)); err == nil {
			t.Errorf("loadConfig accepted:\n%s", section)
		}
	}
}
