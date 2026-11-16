package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// fetcher is the only way the hive reaches hosts that agents name: library
// CDNs, image URLs and live-data hosts. Agents choose those URLs, so every
// connection is checked at dial time, after DNS, against the address it
// actually goes to. That is what stops a thread from reading the panel's
// loopback services, the cloud metadata address or anything else private.
type fetcher struct {
	client *http.Client
	// allowPrivate lets tests reach httptest servers on loopback.
	allowPrivate bool

	mu    sync.Mutex
	cache map[string]cachedFetch
	now   func() time.Time
}

type cachedFetch struct {
	res     *fetched
	expires time.Time
}

type fetched struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        []byte `json:"-"`
}

func newFetcher(now func() time.Time) *fetcher {
	f := &fetcher{cache: map[string]cachedFetch{}, now: now}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: f.guard}
	f.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil, // a proxy would dial on our behalf and bypass the guard
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  10 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
			MaxIdleConns:           8,
			IdleConnTimeout:        30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !f.allowPrivate {
				return errors.New("redirected away from https")
			}
			return nil
		},
	}
	return f
}

// guard runs for every outgoing connection with the resolved address.
func (f *fetcher) guard(network, address string, _ syscall.RawConn) error {
	if f.allowPrivate {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !publicAddr(ap.Addr()) {
		return fmt.Errorf("%s is not a public address", address)
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

// parseFetchURL accepts an absolute https URL without credentials.
func (f *fetcher) parseFetchURL(raw string) (*url.URL, *Error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil {
		return nil, errInvalid("%q is not an absolute URL", raw)
	}
	if u.Scheme != "https" && !(f.allowPrivate && u.Scheme == "http") {
		return nil, errInvalid("%q must be an https URL", raw)
	}
	u.Fragment = ""
	return u, nil
}

// get fetches one URL without cookies or credentials, reading at most max
// bytes. A body longer than that is an error, not a silent truncation.
func (f *fetcher) get(ctx context.Context, u *url.URL, accept string, max int64) (*fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "agentic-hive/"+appVersion)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("the response is larger than %d bytes", max)
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return &fetched{Status: resp.StatusCode, ContentType: strings.ToLower(strings.TrimSpace(ct)), Body: body}, nil
}

// getCached is get with a short memory, so a thread that many people open,
// or that polls, does not turn the hive into a load generator.
func (f *fetcher) getCached(ctx context.Context, u *url.URL, accept string, max int64, ttl time.Duration) (*fetched, error) {
	key := u.String()
	now := f.now()
	f.mu.Lock()
	if c, ok := f.cache[key]; ok && now.Before(c.expires) {
		f.mu.Unlock()
		return c.res, nil
	}
	f.mu.Unlock()
	res, err := f.get(ctx, u, accept, max)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if len(f.cache) >= 200 {
		for k, c := range f.cache {
			if !now.Before(c.expires) {
				delete(f.cache, k)
			}
		}
		if len(f.cache) >= 200 {
			f.cache = map[string]cachedFetch{}
		}
	}
	f.cache[key] = cachedFetch{res: res, expires: now.Add(ttl)}
	f.mu.Unlock()
	return res, nil
}
