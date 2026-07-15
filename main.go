package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	cfgPath := flag.String("config", "config.toml", "path to config file")
	flag.Parse()

	transport := newTransport()

	h := &hub{}
	cfg, err := h.reload(*cfgPath, transport)
	if err != nil {
		log.Fatalf("config: %v", err)
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
	}

	log.Printf("listening on %s", cfg.Listen)
	log.Fatal(srv.ListenAndServe())
}
