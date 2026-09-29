package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// transportFor builds an http.RoundTripper honoring a proxy URL.
// Supported: ""/"direct", socks5://[user:pass@]host:port, http(s)://[user:pass@]host:port.
func transportFor(proxyURL string) (http.RoundTripper, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || strings.EqualFold(proxyURL, "direct") {
		return &http.Transport{
			Proxy:               nil,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{Timeout: 15 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		return &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}, nil
	case "http", "https":
		return &http.Transport{
			Proxy:               http.ProxyURL(u),
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}
}

// clientCache memoizes *http.Client per proxy URL so connections reuse.
type clientCache struct {
	mu      sync.Mutex
	clients map[string]*http.Client
	timeout time.Duration
}

func newClientCache(timeout time.Duration) *clientCache {
	return &clientCache{clients: map[string]*http.Client{}, timeout: timeout}
}

func (c *clientCache) clientFor(proxyURL string) (*http.Client, error) {
	key := strings.TrimSpace(proxyURL)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[key]; ok {
		return cl, nil
	}
	rt, err := transportFor(key)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{Transport: rt, Timeout: c.timeout}
	c.clients[key] = cl
	return cl, nil
}
