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

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	transport := newTransport()

	mux := http.NewServeMux()
	for _, pc := range cfg.Pools {
		p, err := newPool(pc, cfg.Secret, transport)
		if err != nil {
			log.Fatalf("pool %s: %v", pc.Path, err)
		}
		mux.Handle("POST "+pc.Path, p)
		log.Printf("pool %s -> %d upstream(s)", pc.Path, len(pc.Upstreams))
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		// Guards against slowloris. No WriteTimeout on purpose: a browser-based
		// auth server can take a while to return a login code, and Dragonite's
		// own remote_auth_timeout_seconds already bounds the request.
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("listening on %s", cfg.Listen)
	log.Fatal(srv.ListenAndServe())
}
