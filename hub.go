package main

import (
	"crypto/subtle"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

const secretHeader = "X-Remote-Auth-Secret"

// newTransport mirrors the transport Dragonite uses to call remote auth
// directly: single use connections, and no proxy from the environment. Cloning
// http.DefaultTransport instead would inherit HTTP_PROXY and quietly route
// account credentials through it, which calling the auth server direct never
// did. Auth calls are slow and rare, so pooling buys nothing worth that.
func newTransport() *http.Transport {
	return &http.Transport{DisableKeepAlives: true}
}

type upstream struct {
	url    *url.URL
	secret string
}

type pool struct {
	path      string
	inSecret  string
	upstreams []upstream
	rr        atomic.Uint64
	proxy     *httputil.ReverseProxy
}

func newPool(pc poolConfig, inSecret string, transport http.RoundTripper) (*pool, error) {
	p := &pool{path: pc.Path, inSecret: inSecret}

	for _, uc := range pc.Upstreams {
		u, err := url.Parse(uc.Url)
		if err != nil {
			return nil, err
		}
		p.upstreams = append(p.upstreams, upstream{url: u, secret: uc.Secret})
	}

	p.proxy = &httputil.ReverseProxy{
		Transport:    transport,
		Rewrite:      p.rewrite,
		ErrorHandler: p.errorHandler,
	}
	return p, nil
}

// next returns the next upstream, round robin.
func (p *pool) next() upstream {
	i := p.rr.Add(1) - 1
	return p.upstreams[i%uint64(len(p.upstreams))]
}

// rewrite retargets the request at the next upstream. Dragonite POSTs straight
// at remote_auth_url, so the upstream URL replaces ours whole rather than being
// joined onto it.
func (p *pool) rewrite(r *httputil.ProxyRequest) {
	u := p.next()

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

// errorHandler answers when an upstream is unreachable.
//
// Dragonite ignores the HTTP status and reads only the JSON body. status
// INVALID makes it call account.MarkInvalid() and BANNED makes it call
// account.MarkAuthBanned() — both permanent. A transport failure here is our
// problem, not the account's, so status must never be either of those. An
// empty login_code lands on Dragonite's retryable ErrAuthNoToken instead.
func (p *pool) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("pool %s: upstream error: %v", p.path, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, `{"login_code":"","status":"ERROR"}`)
}

func (p *pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := []byte(r.Header.Get(secretHeader))
	want := []byte(p.inSecret)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		// Plain text, not the JSON shape: a wrong secret is a config error and
		// should be loud in Dragonite's log, not a quiet retryable auth miss.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p.proxy.ServeHTTP(w, r)
}
