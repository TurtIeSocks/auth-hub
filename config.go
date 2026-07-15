package main

import (
	"fmt"
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
}

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
		return nil, fmt.Errorf("secret is required (Dragonite sends it as remote_auth_secret)")
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
		}
	}

	return &cfg, nil
}
