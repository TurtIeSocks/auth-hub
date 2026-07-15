package main

import (
	"fmt"
	"testing"
)

func ptr(i int) *int { return &i }

// counts tallies how many of a cycle's slots each upstream gets.
func counts(order []int, n int) []int {
	got := make([]int, n)
	for _, i := range order {
		got[i]++
	}
	return got
}

// The ratio is the whole point: over one cycle each upstream gets exactly its
// weight's share.
func TestWeightedOrderHonoursRatio(t *testing.T) {
	for _, weights := range [][]int{
		{1, 1},
		{3, 1},
		{5, 1, 1},
		{2, 3, 5},
		{1},
		{7, 7},
		{10, 1},
	} {
		order := weightedOrder(weights)

		total := 0
		for _, w := range weights {
			total += w
		}
		if len(order) != total {
			t.Errorf("weights %v: cycle is %d long, want %d", weights, len(order), total)
		}
		for i, got := range counts(order, len(weights)) {
			if got != weights[i] {
				t.Errorf("weights %v: upstream %d got %d slots, want %d", weights, i, got, weights[i])
			}
		}
	}
}

// Equal weights must come out as plain round robin, so that every config
// written before weights existed behaves exactly as it did.
func TestEqualWeightsArePlainRoundRobin(t *testing.T) {
	for n := 1; n <= 5; n++ {
		weights := make([]int, n)
		for i := range weights {
			weights[i] = 1
		}
		want := make([]int, n)
		for i := range want {
			want[i] = i
		}
		if got := weightedOrder(weights); !equalInts(got, want) {
			t.Errorf("%d equal weights: order = %v, want %v", n, got, want)
		}
	}
}

// Smooth, not clumped: 3 and 1 must give a,a,b,a rather than a,a,a,b, so a
// heavy upstream's turns are spread through the cycle.
func TestWeightedOrderIsSmoothNotClumped(t *testing.T) {
	if got, want := weightedOrder([]int{3, 1}), []int{0, 0, 1, 0}; !equalInts(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}

	// The strong version: no upstream ever takes more consecutive turns than a
	// clumped layout would force. For 5:1:1 a clumped cycle starts with 5 in a
	// row; smooth must do better.
	order := weightedOrder([]int{5, 1, 1})
	run, worst := 1, 1
	for i := 1; i < len(order); i++ {
		if order[i] == order[i-1] {
			run++
		} else {
			run = 1
		}
		if run > worst {
			worst = run
		}
	}
	if worst >= 5 {
		t.Errorf("longest run of one upstream is %d in %v; that's clumped", worst, order)
	}
}

// The cycle has to be repeatable: running totals return to zero, so the ratio
// holds across cycles instead of drifting.
func TestWeightedOrderCyclesCleanly(t *testing.T) {
	weights := []int{3, 1, 2}
	one := weightedOrder(weights)

	// Two cycles must be the first cycle twice: proof the state resets.
	two := append(append([]int{}, one...), one...)
	for i, got := range counts(two, len(weights)) {
		if got != weights[i]*2 {
			t.Errorf("over two cycles upstream %d got %d, want %d", i, got, weights[i]*2)
		}
	}
}

// Weight decides which upstream a request starts on, in the configured ratio.
func TestPickHonoursWeightAcrossRequests(t *testing.T) {
	p, err := newPool(poolConfig{Path: "/ptc", Upstreams: []upstreamConfig{
		{Url: "http://heavy.invalid", Weight: ptr(3)},
		{Url: "http://light.invalid", Weight: ptr(1)},
	}}, "s", nil)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	for i := range 400 {
		got[p.pick(&attempt{start: uint64(i)}).url.Host]++
	}
	if got["heavy.invalid"] != 300 || got["light.invalid"] != 100 {
		t.Errorf("distribution = %v, want heavy 300 / light 100", got)
	}
}

// The interaction that's easy to get wrong: weights repeat an upstream in the
// rotation, so a retry must walk upstreams rather than rotation slots, or it
// would land straight back on the heavy upstream that just failed.
func TestFailoverNeverRepeatsAnUpstreamDespiteWeights(t *testing.T) {
	p, err := newPool(poolConfig{Path: "/ptc", Upstreams: []upstreamConfig{
		{Url: "http://a.invalid", Weight: ptr(5)},
		{Url: "http://b.invalid", Weight: ptr(1)},
		{Url: "http://c.invalid", Weight: ptr(1)},
	}}, "s", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Whatever slot a request starts on, its tries must cover every upstream
	// exactly once.
	for start := range len(p.order) {
		seen := map[string]bool{}
		for n := range len(p.upstreams) {
			host := p.pick(&attempt{start: uint64(start), n: n}).url.Host
			if seen[host] {
				t.Fatalf("start %d: tried %s twice", start, host)
			}
			seen[host] = true
		}
		if len(seen) != len(p.upstreams) {
			t.Fatalf("start %d: covered %d upstreams, want %d", start, len(seen), len(p.upstreams))
		}
	}
}

// Weight 0 drains an upstream: no traffic, and not even as a failover target.
func TestZeroWeightUpstreamGetsNothing(t *testing.T) {
	p, err := newPool(poolConfig{Path: "/ptc", Upstreams: []upstreamConfig{
		{Url: "http://live.invalid", Weight: ptr(1)},
		{Url: "http://drained.invalid", Weight: ptr(0)},
	}}, "s", nil)
	if err != nil {
		t.Fatal(err)
	}

	for start := range 20 {
		for n := range len(p.upstreams) {
			if host := p.pick(&attempt{start: uint64(start), n: n}).url.Host; host == "drained.invalid" {
				t.Fatalf("a drained upstream was picked (start %d, try %d)", start, n)
			}
		}
	}
}

// An omitted weight means 1, so configs written before weights existed keep
// their exact behaviour.
func TestOmittedWeightDefaultsToOne(t *testing.T) {
	if got := (upstreamConfig{Url: "http://a"}).weight(); got != 1 {
		t.Errorf("omitted weight = %d, want 1", got)
	}
	if got := (upstreamConfig{Url: "http://a", Weight: ptr(0)}).weight(); got != 0 {
		t.Errorf("explicit 0 = %d, want 0 (drained, not defaulted)", got)
	}
}

func TestWeightValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		weights []*int
		wantErr bool
	}{
		{"negative", []*int{ptr(-1)}, true},
		{"over the maximum", []*int{ptr(maxWeight + 1)}, true},
		{"at the maximum", []*int{ptr(maxWeight)}, false},
		{"every upstream drained", []*int{ptr(0), ptr(0)}, true},
		{"one drained, one live", []*int{ptr(0), ptr(1)}, false},
		{"omitted", []*int{nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "listen = \":1\"\nsecret = \"s\"\n\n[[pool]]\npath = \"/ptc\"\n"
			for _, w := range tc.weights {
				body += fmt.Sprintf("\n  [[pool.upstream]]\n  url = \"http://a:1/x\"\n")
				if w != nil {
					body += fmt.Sprintf("  weight = %d\n", *w)
				}
			}
			path := writeTemp(t, body)

			_, err := loadConfig(path)
			if tc.wantErr && err == nil {
				t.Error("want an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want no error, got %v", err)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
