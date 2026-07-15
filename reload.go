package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// pollInterval is how often the config file is checked for changes.
const pollInterval = 5 * time.Second

// reload reads the config and swaps in a fresh set of pools. The current pools
// are left alone unless the new config both parses and builds, so a typo can't
// take auth logins down — it just gets logged.
func (h *hub) reload(path string, transport http.RoundTripper) (*config, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, err
	}

	pools := make(map[string]*pool, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		p, err := newPool(pc, cfg.Secret, transport)
		if err != nil {
			return nil, fmt.Errorf("pool %s: %w", pc.Path, err)
		}
		pools[pc.Path] = p
		log.Printf("pool %s -> %d upstream(s)", pc.Path, len(pc.Upstreams))
	}

	h.pools.Store(&pools)
	return cfg, nil
}

// watch reloads on SIGHUP, the way Dragonite does, and also whenever the config
// file changes on disk.
//
// It polls rather than using fsnotify, because the two fail differently. A
// missed poll is a reload that lands a few seconds late. A missed inotify event
// is a reload that never happens and says nothing about it — and inotify is the
// part that doesn't reliably survive the way this gets deployed, with the config
// edited on the host and mounted into a container. Being late is recoverable;
// being silent isn't. Polling is also stdlib, where fsnotify is a dependency.
//
// Its own limit: a filesystem with coarse mtime granularity could miss a
// same-size rewrite inside one tick. stamp keys on size as well to narrow that,
// and SIGHUP is there when you want the reload now regardless.
func (h *hub) watch(path string, transport http.RoundTripper) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	last := stamp(path)

	for {
		select {
		case <-sighup:
			log.Printf("SIGHUP: reloading %s", path)
		case <-tick.C:
			cur := stamp(path)
			if cur == last {
				continue
			}
			// Recorded before the reload is attempted, so a config that fails
			// to load is reported once rather than every tick until it's fixed.
			last = cur
			log.Printf("%s changed: reloading", path)
		}

		cfg, err := h.reload(path, transport)
		if err != nil {
			log.Printf("reload failed, keeping the previous config: %v", err)
			continue
		}
		if cfg.Listen != h.listen {
			log.Printf("reload: listen is now %q but auth-hub is bound to %q; restart to move it", cfg.Listen, h.listen)
		}
		log.Printf("reload done")
	}
}

// stamp identifies a version of the config file. Size is in there alongside
// mtime to catch a same-timestamp rewrite, and a file that can't be statted
// stamps empty, so it reloads once it comes back.
func stamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}
