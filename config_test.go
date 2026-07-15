package main

import "testing"

func TestShippedExampleParses(t *testing.T) {
	cfg, err := loadConfig("config.toml.example")
	if err != nil {
		t.Fatalf("config.toml.example does not load: %v", err)
	}
	if cfg.Listen != ":9090" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if len(cfg.Pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(cfg.Pools))
	}
	p := cfg.Pools[0]
	if p.Path != "/ptc" {
		t.Errorf("path = %q", p.Path)
	}
	if len(p.Upstreams) != 2 {
		t.Fatalf("upstreams = %d, want 2 (nesting broken?)", len(p.Upstreams))
	}
	if p.Upstreams[0].Secret != "auth-1-secret" || p.Upstreams[1].Secret != "auth-2-secret" {
		t.Errorf("secrets = %q / %q", p.Upstreams[0].Secret, p.Upstreams[1].Secret)
	}
	t.Logf("parsed OK: %s -> %v", p.Path, []string{p.Upstreams[0].Url, p.Upstreams[1].Url})
}
