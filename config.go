package main

import (
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/pelletier/go-toml/v2"
)

type config struct {
	Listen string       `toml:"listen"`
	Secret string       `toml:"secret"`
	Pools  []poolConfig `toml:"pool"`
}

type poolConfig struct {
	Path      string           `toml:"path"`
	Upstreams []upstreamConfig `toml:"upstream"`
}

type upstreamConfig struct {
	Url    string `toml:"url"`
	Secret string `toml:"secret"`
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

	// The inbound secret is the only thing standing between the internet and a
	// pool of working auth servers. An empty one would make this an open relay.
	if cfg.Secret == "" {
		log.Printf("the secret was not set, auth-hub might be vulnerable to the world!")
		// return nil, fmt.Errorf("secret is required (Dragonite sends it as remote_auth_secret)")
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
