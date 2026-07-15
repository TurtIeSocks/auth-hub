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
	opts := &slog.HandlerOptions{Level: level}
	return slog.New(splitHandler{
		out: slog.NewTextHandler(&out, opts),
		err: slog.NewTextHandler(&err, opts),
	}), &out, &err
}

// The whole point of the split: stdout carries what happened, stderr carries
// only what went wrong.
func TestSplitHandlerRoutesByLevel(t *testing.T) {
	log, out, errBuf := testLogger(slog.LevelDebug)

	log.Debug("to-out-debug")
	log.Info("to-out-info")
	log.Warn("to-err-warn")
	log.Error("to-err-error")

	for _, want := range []string{"to-out-debug", "to-out-info"} {
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

// Below the configured level, nothing is written at all — including to the file
// the writers may be teed into.
func TestLevelFilters(t *testing.T) {
	log, out, _ := testLogger(slog.LevelInfo)
	log.Debug("debug-msg")
	log.Info("info-msg")

	if strings.Contains(out.String(), "debug-msg") {
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
		"debug": slog.LevelDebug,
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
	for _, gone := range []string{"verbose", "trace"} {
		if _, err := parseLevel(gone); err == nil {
			t.Errorf("parseLevel accepted %q, which is not a level", gone)
		}
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
	if cfg.Log.SaveToFile {
		t.Error("save_to_file defaulted on, want the streams alone")
	}
}

// The date in the name is the only rotation there is, so a hub that runs past
// midnight has to move to the new day's file rather than writing to yesterday's
// for ever.
func TestDailyWriterRollsOverAtMidnight(t *testing.T) {
	inTempLogDir(t)

	w := &dailyWriter{}
	for _, day := range []string{"2026-07-14", "2026-07-15"} {
		if err := w.open(day); err != nil {
			t.Fatal(err)
		}
		if _, err := w.f.WriteString(day + " line\n"); err != nil {
			t.Fatal(err)
		}
	}

	for day, want := range map[string]string{
		"2026-07-14": "2026-07-14 line\n",
		"2026-07-15": "2026-07-15 line\n",
	} {
		got, err := os.ReadFile(filepath.Join(logDir, "auth-hub-"+day+".log"))
		if err != nil {
			t.Fatalf("%s: %v", day, err)
		}
		if string(got) != want {
			t.Errorf("auth-hub-%s.log = %q, want %q", day, got, want)
		}
	}
}

// Write picks the day itself, and appends rather than truncating, so a restart
// doesn't eat the earlier part of the day.
func TestDailyWriterWritesTodayAndAppends(t *testing.T) {
	inTempLogDir(t)

	for _, line := range []string{"first\n", "second\n"} {
		w := &dailyWriter{} // a fresh one each time, as a restart would be
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	name := filepath.Join(logDir, "auth-hub-"+today()+".log")
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("nothing at %s: %v", name, err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("%s = %q, want both lines", name, got)
	}
}

// inTempLogDir runs the test in a scratch working directory, since logDir is
// relative to it.
func inTempLogDir(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
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
