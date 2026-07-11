// Package delegate proxies whole quote requests to the php backend's /api/v2/quote — go-ba's
// fallback for request classes it does not handle natively (auth, coupons, van/FTL, declared
// values, FedEx-addressed, unknown sources) and for any internal failure. The php answer is
// returned verbatim, so go-ba never serves a wrong or improvised response.
package delegate

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Delegation reasons (metric labels; each disable-able via GO_BA_DELEGATE_DISABLE csv).
const (
	ReasonAuth           = "auth"
	ReasonCoupon         = "coupon"
	ReasonVanFTL         = "vanftl"
	ReasonCourierPin     = "courier-pin"
	ReasonSource         = "source"
	ReasonCurrency       = "currency"
	ReasonValue          = "value"
	ReasonFedexAddressed = "fedex-addressed"
	ReasonError          = "error" // catch-all: native path failed
)

// LoopGuardHeader marks proxied requests; receiving one back means a routing loop.
const LoopGuardHeader = "X-GoBa-Delegated"

// Counters for /metrics (label -> count).
var (
	counts    = map[string]*atomic.Int64{}
	errCount  atomic.Int64
	loopCount atomic.Int64
)

func init() {
	for _, r := range []string{ReasonAuth, ReasonCoupon, ReasonVanFTL, ReasonCourierPin,
		ReasonSource, ReasonCurrency, ReasonValue, ReasonFedexAddressed, ReasonError} {
		counts[r] = &atomic.Int64{}
	}
}

// Count increments the delegation counter for a reason.
func Count(reason string) {
	if c, ok := counts[reason]; ok {
		c.Add(1)
	}
}

// CountLoop increments the loop-guard counter.
func CountLoop() { loopCount.Add(1) }

// Metrics renders the delegate counters in the hand-rolled /metrics text format.
func Metrics(w io.Writer) {
	for r, c := range counts {
		io.WriteString(w, "go_ba_delegated_total{reason=\""+r+"\"} "+itoa(c.Load())+"\n")
	}
	io.WriteString(w, "go_ba_delegate_errors_total "+itoa(errCount.Load())+"\n")
	io.WriteString(w, "go_ba_delegate_loop_total "+itoa(loopCount.Load())+"\n")
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Proxy forwards quote requests to the php backend.
type Proxy struct {
	baseURL    string
	hostHeader string // devbox: nginx routes by server_name (be.docker.localhost)
	client     *http.Client
	disabled   map[string]bool
}

// NewProxyFromEnv builds the proxy from GO_BA_PHP_URL / GO_BA_PHP_HOST_HEADER /
// GO_BA_PHP_TIMEOUT / GO_BA_DELEGATE_DISABLE. Returns nil when GO_BA_PHP_URL is unset
// (delegation off; predicate reasons are still counted by the caller).
func NewProxyFromEnv() *Proxy {
	base := os.Getenv("GO_BA_PHP_URL")
	if base == "" {
		return nil
	}
	timeout := 60 * time.Second
	if t := os.Getenv("GO_BA_PHP_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}
	disabled := map[string]bool{}
	for _, r := range strings.Split(os.Getenv("GO_BA_DELEGATE_DISABLE"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			disabled[r] = true
		}
	}
	return &Proxy{
		baseURL:    strings.TrimRight(base, "/"),
		hostHeader: os.Getenv("GO_BA_PHP_HOST_HEADER"),
		client:     &http.Client{Timeout: timeout},
		disabled:   disabled,
	}
}

// Enabled reports whether delegation for this reason is active.
func (p *Proxy) Enabled(reason string) bool {
	return p != nil && !p.disabled[reason]
}

// forwarded request headers (php resolves the user, language and warnings semantics from these).
var forwardHeaders = []string{"Authorization", "x-api-key", "Accept-Language", "Origin", "Content-Type", "User-Agent"}

// Quote forwards the original request body/headers to php POST /api/v2/quote and writes the php
// response (status, content type, body) verbatim to w. Returns false when the transport failed
// (a 503 problem response has then been written).
func (p *Proxy) Quote(w http.ResponseWriter, r *http.Request, body []byte, reason string) bool {
	return p.forward(w, r, http.MethodPost, "/api/v2/quote", body, reason)
}

// Get forwards a GET request (query string included) to the php backend.
func (p *Proxy) Get(w http.ResponseWriter, r *http.Request, pathAndQuery string, reason string) bool {
	return p.forward(w, r, http.MethodGet, pathAndQuery, nil, reason)
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, method, pathAndQuery string, body []byte, reason string) bool {
	Count(reason)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, p.baseURL+pathAndQuery, reader)
	if err != nil {
		return p.fail(w, err)
	}
	for _, h := range forwardHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if p.hostHeader != "" {
		req.Host = p.hostHeader
	}
	req.Header.Set(LoopGuardHeader, "1")
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			xff += ", " + ip
		} else {
			xff = ip
		}
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return p.fail(w, err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

func (p *Proxy) fail(w http.ResponseWriter, err error) bool {
	errCount.Add(1)
	w.Header().Set("Content-Type", "application/api-problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"jsonApi":{"version":"2.0"},"title":"Service Unavailable"}`)
	_ = err
	return false
}
