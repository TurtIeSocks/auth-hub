package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

const secretHeader = "X-Remote-Auth-Secret"

// maxBodyBytes caps what we buffer per request so a retry can replay it.
// Dragonite's login request is a few hundred bytes of JSON.
const maxBodyBytes = 64 << 10

// newTransport mirrors the transport Dragonite uses to call remote auth
// directly: single use connections, and no proxy from the environment. Cloning
// http.DefaultTransport instead would inherit HTTP_PROXY and quietly route
// account credentials through it, which calling the auth server direct never
// did. Auth calls are slow and rare, so pooling buys nothing worth that.
func newTransport() *http.Transport {
	return &http.Transport{DisableKeepAlives: true}
}

// hub routes requests to pools by path. The whole set is swapped atomically on
// reload, so a config change never interrupts a request in flight: one already
// dispatched keeps the pool it started with.
type hub struct {
	pools  atomic.Pointer[map[string]*pool]
	listen string // the address actually bound, to notice a reload trying to change it
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The rejections below log at debug rather than warn on purpose. Every one
	// of them is reachable by anything that can open a socket to us, so logging
	// them by default would hand a stranger a write amplifier pointed at the
	// disk. At debug they're there when you go looking, which is when a pool
	// path or a secret has been typed wrong and you need to see it land.
	p, ok := (*h.pools.Load())[r.URL.Path]
	if !ok {
		slog.Warn("no pool for path", "path", r.URL.Path, "remote", r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		slog.Warn("method not allowed", "path", r.URL.Path, "method", r.Method, "remote", r.RemoteAddr)
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.ServeHTTP(w, r)
}

type upstream struct {
	url    *url.URL
	secret string
}

type pool struct {
	path     string
	inSecret string
	// upstreams holds only the ones that can serve: weight 0 is drained and
	// never appears, so failover can't wander onto it either.
	upstreams []upstream
	// order is one full cycle of the weighted rotation, as indices into
	// upstreams. Precomputed, so picking is an array read rather than a lock.
	order []int
	rr    atomic.Uint64
	proxy *httputil.ReverseProxy
}

// attempt is the per-request state. ReverseProxy hands rewrite and errorHandler
// only a request, so it rides along in the request context.
type attempt struct {
	in    *http.Request // the original inbound request, cloned for each try
	body  []byte
	start uint64 // this request's slot in the round robin
	n     int    // which try this is, 0-based
}

type attemptKey struct{}

func newPool(pc poolConfig, inSecret string, transport http.RoundTripper) (*pool, error) {
	p := &pool{path: pc.Path, inSecret: inSecret}

	var weights []int
	for _, uc := range pc.Upstreams {
		u, err := url.Parse(uc.Url)
		if err != nil {
			return nil, err
		}
		w := uc.weight()
		if w == 0 {
			slog.Info("upstream is drained (weight 0)", "pool", pc.Path, "upstream", u.Host)
			continue
		}
		p.upstreams = append(p.upstreams, upstream{url: u, secret: uc.Secret})
		weights = append(weights, w)
	}
	if len(p.upstreams) == 0 {
		return nil, fmt.Errorf("no upstream with a weight above 0")
	}
	p.order = weightedOrder(weights)

	p.proxy = &httputil.ReverseProxy{
		Transport:    transport,
		Rewrite:      p.rewrite,
		ErrorHandler: p.errorHandler,
		// Left unset, this is the log package's default logger, which
		// slog.SetDefault points back at our handler — at info, onto stdout.
		// The things it reports are upstreams misbehaving mid-response, which
		// errorHandler never sees because the reply has already started. Those
		// belong on stderr with the rest of the bad news.
		ErrorLog: warnLog(),
	}
	return p, nil
}

// weightedOrder returns one full cycle of the rotation as upstream indices,
// each appearing weight times. It's nginx's smooth weighted round robin, run
// ahead of time: smooth means the turns of a heavy upstream are spread through
// the cycle rather than clumped at the front, so weights 3 and 1 give a,a,b,a
// instead of a,a,a,b and three concurrent logins don't all pile onto one host.
//
// Equal weights come out as plain round robin, so an unweighted config behaves
// exactly as it did before weights existed. The cycle is self contained — the
// running totals return to zero — so repeating it holds the ratio for ever.
func weightedOrder(weights []int) []int {
	total := 0
	for _, w := range weights {
		total += w
	}

	current := make([]int, len(weights))
	order := make([]int, 0, total)
	for range total {
		best := 0
		for i, w := range weights {
			current[i] += w
			if current[i] > current[best] {
				best = i
			}
		}
		current[best] -= total
		order = append(order, best)
	}
	return order
}

// pick returns the upstream for this try. The first try reads the request's own
// slot in the weighted rotation; a retry walks on from there through upstreams,
// not through the rotation, so it lands on a different upstream every time
// rather than on another of a heavy upstream's turns.
func (p *pool) pick(a *attempt) upstream {
	first := p.order[a.start%uint64(len(p.order))]
	return p.upstreams[(first+a.n)%len(p.upstreams)]
}

// dispatch sends one try through the proxy.
func (p *pool) dispatch(w http.ResponseWriter, a *attempt) {
	r := a.in.Clone(context.WithValue(a.in.Context(), attemptKey{}, a))
	r.Body = io.NopCloser(bytes.NewReader(a.body))
	r.ContentLength = int64(len(a.body))
	p.proxy.ServeHTTP(w, r)
}

// rewrite retargets the request at this try's upstream. Dragonite POSTs
// straight at remote_auth_url, so the upstream URL replaces ours whole rather
// than being joined onto it.
func (p *pool) rewrite(r *httputil.ProxyRequest) {
	a := r.In.Context().Value(attemptKey{}).(*attempt)
	u := p.pick(a)

	// The one message per try, and the only place that says which upstream a
	// given login actually went to — the thing you want when one of them is
	// lying. Logins are slow and rare, so this stays quiet even at debug.
	slog.Info("dispatching", "pool", p.path, "upstream", u.url.Host, "try", a.n+1)

	r.Out.URL.Scheme = u.url.Scheme
	r.Out.URL.Host = u.url.Host
	r.Out.URL.Path = u.url.Path
	r.Out.URL.RawQuery = u.url.RawQuery
	r.Out.Host = u.url.Host

	// Swap Dragonite's secret for this upstream's. Never forward ours.
	if u.secret == "" {
		r.Out.Header.Del(secretHeader)
	} else {
		r.Out.Header.Set(secretHeader, u.secret)
	}
}

// errorHandler runs when a try fails at the transport level: refused, reset,
// timed out. ReverseProxy calls it before anything is written to w, so we are
// still free to try another upstream.
//
// It never reports INVALID or BANNED. Dragonite ignores the HTTP status and
// reads only the JSON body, and answers INVALID with account.MarkInvalid() and
// BANNED with account.MarkAuthBanned() — both permanent. A connection problem
// is ours, not the account's. An empty login_code lands on Dragonite's
// retryable ErrAuthNoToken instead.
func (p *pool) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	a := r.Context().Value(attemptKey{}).(*attempt)
	failed := p.pick(a).url.Host

	// A cancelled context means Dragonite hung up or spent its
	// remote_auth_timeout_seconds. Another upstream cannot help, and trying one
	// would burn a login on a caller that has already stopped listening.
	if a.n+1 < len(p.upstreams) && r.Context().Err() == nil {
		slog.Warn("upstream failed, trying the next one",
			"pool", p.path, "upstream", failed, "error", err)
		p.dispatch(w, &attempt{in: a.in, body: a.body, start: a.start, n: a.n + 1})
		return
	}

	// Two different things end up here, and only one of them is an upstream's
	// fault. Reporting a caller that timed out as an upstream failure would
	// point at a host that never did anything wrong, at the level people wire
	// alerts to.
	if r.Context().Err() != nil {
		slog.Warn("caller gave up before an upstream answered",
			"pool", p.path, "upstream", failed, "error", err, "tries", a.n+1)
	} else {
		slog.Error("every upstream failed",
			"pool", p.path, "upstream", failed, "error", err, "tries", a.n+1)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, `{"login_code":"","status":"ERROR"}`)
}

func (p *pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := []byte(r.Header.Get(secretHeader))
	want := []byte(p.inSecret)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		// Never the secrets themselves, not even the one that was offered: a
		// near miss is still most of a working secret, and the log is a file
		// with wider reach than the config it came from.
		slog.Warn("wrong secret", "pool", p.path, "remote", r.RemoteAddr, "sent_one", len(got) > 0)
		// Plain text, not the JSON shape: a wrong secret is a config error and
		// should be loud in Dragonite's log, not a quiet retryable auth miss.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Buffered up front so a failed try can be replayed against the next
	// upstream: the proxied body is a stream that can only be read once.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	p.dispatch(w, &attempt{in: r, body: body, start: p.rr.Add(1) - 1})
}
