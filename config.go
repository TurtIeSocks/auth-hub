package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type config struct {
	Listen string       `toml:"listen"`
	Secret string       `toml:"secret"`
	Log    logConfig    `toml:"log"`
	Pools  []poolConfig `toml:"pool"`
}

type logConfig struct {
	// Level is debug, info, warn or error. Reloadable.
	Level string `toml:"level"`
	// Format is text or json. text reads well over docker logs; json is for
	// when something downstream is doing the reading.
	Format string `toml:"format"`
	// SaveToFile also appends every line to logs/auth-hub-YYYY-MM-DD.log,
	// alongside the streams rather than instead of them.
	SaveToFile bool `toml:"save_to_file"`
}

type poolConfig struct {
	Path      string           `toml:"path"`
	Upstreams []upstreamConfig `toml:"upstream"`
}

type upstreamConfig struct {
	Url    string `toml:"url"`
	Secret string `toml:"secret"`
	// Label is a friendly name for this upstream in metrics (the `upstream`
	// label) and grafana. Omitted, it falls back to the URL host, so a series
	// always has a value; set it when a host is opaque or you want the graph to
	// read auth-tokyo rather than 10.0.0.4:5090.
	Label string `toml:"label"`
	// Weight is a pointer so that an omitted weight (the common case, and every
	// config written before weights existed) can default to 1 while an explicit
	// 0 still means something different: drained, gets nothing.
	Weight *int `toml:"weight"`
}

// weight is the configured weight, or 1 if it was left out.
func (uc upstreamConfig) weight() int {
	if uc.Weight == nil {
		return 1
	}
	return *uc.Weight
}

// maxWeight bounds the precomputed rotation, which is sum(weights) long. Ratios
// past this are meaningless anyway, and it stops a stray zero in the config
// from asking for a gigabyte of slice.
const maxWeight = 1000

func loadConfig(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg config
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}

	if cfg.Listen == "" {
		cfg.Listen = ":9090"
	}

	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "text"
	}
	// Case isn't the user's problem, and it would be a mean thing to reject a
	// config over.
	cfg.Log.Level = strings.ToLower(cfg.Log.Level)
	cfg.Log.Format = strings.ToLower(cfg.Log.Format)
	// Checked here, with everything else, so that a typo in the log config is
	// caught by the same reload that keeps the last good config rather than
	// taking the logger down with it.
	if _, err := parseLevel(cfg.Log.Level); err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	if cfg.Log.Format != "text" && cfg.Log.Format != "json" {
		return nil, fmt.Errorf("log: format %q is not text or json", cfg.Log.Format)
	}

	if len(cfg.Pools) == 0 {
		return nil, fmt.Errorf("at least one [[pool]] is required")
	}

	seen := map[string]bool{}
	for i, p := range cfg.Pools {
		if p.Path == "" || p.Path[0] != '/' {
			return nil, fmt.Errorf("pool %d: path %q must start with '/'", i, p.Path)
		}
		if seen[p.Path] {
			return nil, fmt.Errorf("pool %d: duplicate path %q", i, p.Path)
		}
		seen[p.Path] = true

		if len(p.Upstreams) == 0 {
			return nil, fmt.Errorf("pool %q: at least one [[pool.upstream]] is required", p.Path)
		}

		live := 0
		for j, u := range p.Upstreams {
			parsed, err := url.Parse(u.Url)
			if err != nil {
				return nil, fmt.Errorf("pool %q upstream %d: %w", p.Path, j, err)
			}
			if parsed.Scheme != "http" && parsed.Scheme != "https" {
				return nil, fmt.Errorf("pool %q upstream %d: url %q needs an http/https scheme", p.Path, j, u.Url)
			}
			if parsed.Host == "" {
				return nil, fmt.Errorf("pool %q upstream %d: url %q has no host", p.Path, j, u.Url)
			}

			switch w := u.weight(); {
			case w < 0:
				return nil, fmt.Errorf("pool %q upstream %d: weight %d cannot be negative", p.Path, j, w)
			case w > maxWeight:
				return nil, fmt.Errorf("pool %q upstream %d: weight %d exceeds the maximum of %d", p.Path, j, w, maxWeight)
			case w > 0:
				live++
			}
		}
		if live == 0 {
			return nil, fmt.Errorf("pool %q: every upstream has weight 0, so nothing can serve it", p.Path)
		}
	}

	return &cfg, nil
}
