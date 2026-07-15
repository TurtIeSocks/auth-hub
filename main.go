package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	cfgPath := flag.String("config", "config.toml", "path to config file")
	flag.Parse()

	transport := newTransport()

	h := &hub{}
	cfg, err := h.reload(*cfgPath, transport)
	if err != nil {
		// Straight to stderr, not through slog: setting the logger up is one of
		// the things this reload does, so it's one of the things that can have
		// just failed.
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	h.listen = cfg.Listen

	go h.watch(*cfgPath, transport)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: h,
		// Guards against slowloris. No WriteTimeout on purpose: a browser-based
		// auth server can take a while to return a login code, and Dragonite's
		// own remote_auth_timeout_seconds already bounds the request.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          warnLog(),
	}

	slog.Info("listening", "addr", cfg.Listen)
	slog.Error("stopped", "error", srv.ListenAndServe())
	os.Exit(1)
}
