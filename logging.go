package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// levelVar carries the level into the handler by reference, so a reload can
// move it without rebuilding anything. It's the one part of the log config that
// can change at runtime — and the one you actually want to, since turning on
// debug to watch a login go wrong is no use if it costs you a restart first.
var levelVar = new(slog.LevelVar)

// current is the log config in force, nil until the logger is installed.
var current *logConfig

var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

func parseLevel(s string) (slog.Level, error) {
	l, ok := levels[strings.ToLower(s)]
	if !ok {
		return 0, fmt.Errorf("level %q is not one of debug, info, warn, error", s)
	}
	return l, nil
}

// setupLogging installs the configured logger as slog's default. On a later
// call it moves the level and nothing else: the writers and the format are
// fixed once the handler is built, like the listen address is once it's bound,
// so a reload that changes them says so rather than pretending.
func setupLogging(c logConfig) error {
	level, err := parseLevel(c.Level)
	if err != nil {
		return err
	}

	if current != nil {
		levelVar.Set(level)
		if c.Format != current.Format || c.File != current.File {
			slog.Warn("log format and file only apply at startup; restart to change them",
				"running_format", current.Format, "running_file", current.File)
		}
		return nil
	}
	levelVar.Set(level)

	out, errOut := io.Writer(os.Stdout), io.Writer(os.Stderr)
	if c.File != "" {
		f, err := os.OpenFile(c.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return fmt.Errorf("log file: %w", err)
		}
		// Added to the streams rather than replacing them: the streams are what
		// docker logs and journald read, and losing those to gain a file would
		// be a bad trade. Never closed, because it lives exactly as long as the
		// process does. Appended to, so a restart doesn't eat the evidence of
		// why the last one stopped.
		//
		// Both handlers write here and each locks only its own stream, so the
		// file needs a lock of its own to keep a warn from landing in the
		// middle of an info's line.
		shared := &syncWriter{w: f}
		out, errOut = io.MultiWriter(out, shared), io.MultiWriter(errOut, shared)
	}

	opts := &slog.HandlerOptions{Level: levelVar}
	build := func(w io.Writer) slog.Handler {
		if c.Format == "json" {
			return slog.NewJSONHandler(w, opts)
		}
		return slog.NewTextHandler(w, opts)
	}

	slog.SetDefault(slog.New(splitHandler{out: build(out), err: build(errOut)}))
	current = &c
	return nil
}

// splitHandler sends warn and error to one handler and everything below it to
// another, so that stdout carries what happened and stderr carries only what
// went wrong. That's the split every shell, container runtime and log shipper
// already knows how to act on, and it's free here: it needs no level field to
// be parsed back out to tell an error from a login.
type splitHandler struct {
	out, err slog.Handler
}

func (h splitHandler) pick(l slog.Level) slog.Handler {
	if l >= slog.LevelWarn {
		return h.err
	}
	return h.out
}

func (h splitHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.pick(l).Enabled(ctx, l)
}

func (h splitHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.pick(r.Level).Handle(ctx, r)
}

// WithAttrs and WithGroup fan out to both, because which one a record lands on
// isn't known until it has a level, and by then it must already be carrying
// whatever the logger was built up with.

func (h splitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return splitHandler{out: h.out.WithAttrs(attrs), err: h.err.WithAttrs(attrs)}
}

func (h splitHandler) WithGroup(name string) slog.Handler {
	return splitHandler{out: h.out.WithGroup(name), err: h.err.WithGroup(name)}
}

// syncWriter serialises writes from the two handlers to the one file they
// share.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// warnLog adapts our handler back into the *log.Logger that net/http still
// takes for its ErrorLog fields. Without one they fall back to the log
// package's default logger, which slog.SetDefault has pointed back at us — so
// they'd still be captured, but as info, on stdout. net/http only writes there
// when something is wrong, so warn is the honest level and stderr the right
// stream.
//
// Called after setupLogging, so the handler it binds is the configured one.
func warnLog() *log.Logger {
	return slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn)
}
