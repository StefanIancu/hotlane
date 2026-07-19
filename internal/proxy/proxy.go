// Package proxy is the traffic front: a reverse proxy whose backend target
// can be swapped atomically. Promote and rollback are both just Set calls,
// which is what makes them sub-second regardless of app size.
package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

// Flipper routes all traffic to the current target backend.
type Flipper struct {
	target atomic.Pointer[url.URL]
	rp     *httputil.ReverseProxy
}

// New returns a Flipper with no target; requests 503 until Set is called.
func New() *Flipper {
	f := &Flipper{}
	f.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(f.target.Load())
			pr.SetXForwarded()
		},
	}
	return f
}

// Set atomically flips the backend to hostport (e.g. "127.0.0.1:55007").
// In-flight requests finish against the old target.
func (f *Flipper) Set(hostport string) {
	f.target.Store(&url.URL{Scheme: "http", Host: hostport})
}

// Target returns the current backend, or "" if none.
func (f *Flipper) Target() string {
	if u := f.target.Load(); u != nil {
		return u.Host
	}
	return ""
}

func (f *Flipper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.target.Load() == nil {
		http.Error(w, "hotlane: no live version yet", http.StatusServiceUnavailable)
		return
	}
	f.rp.ServeHTTP(w, r)
}
