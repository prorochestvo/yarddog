package infrastructure

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

// NewChecker builds a Checker from the resolved connectivity targets and the
// shared probe timeout (design §4). routerAddr is the router's web UI base
// URL (e.g. "http://192.168.1.1"); it is resolved to a dialable host:port
// once here so a malformed ROUTER_ADDR fails fast at construction instead of
// turning every later gateway probe into a silent, unexplained "down".
func NewChecker(ipTargets, domainTargets []string, routerAddr string, timeout time.Duration) (*Checker, error) {
	gatewayTarget, err := hostPortFromURL(routerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve router address %q: %w", routerAddr, err)
	}

	return &Checker{
		ipTargets:     ipTargets,
		domainTargets: domainTargets,
		gatewayTarget: gatewayTarget,
		timeout:       timeout,
		httpClient: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// a redirect away from a generate_204-style endpoint is not
				// itself a 2xx/204 success; returning the redirect response
				// as-is (instead of following it) lets the status check in
				// probeHTTP fail it honestly rather than reporting a false up.
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Checker probes the connectivity targets resolved from Config (design §4)
// and the router's gateway (design §5) over a shared, timeout-bound
// http.Client. Its Check and Gateway methods implement services.Checker.
type Checker struct {
	ipTargets     []string
	domainTargets []string
	gatewayTarget string
	timeout       time.Duration
	httpClient    *http.Client
}

var _ services.Checker = (*Checker)(nil)

// Check probes every ip and domain target concurrently — one goroutine per
// target, each writing to its own slice index so no mutex is needed — and
// hands the results to domain.Verdict for the quorum decision (design §4.2):
// the internet is down when all ip targets fail or all domain targets fail.
func (c *Checker) Check(ctx context.Context) domain.Result {
	results := make([]domain.TargetResult, len(c.ipTargets)+len(c.domainTargets))

	var wg sync.WaitGroup
	for i, target := range c.ipTargets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			results[i] = c.probeTCP(ctx, target, domain.CheckKindIP)
		}(i, target)
	}
	offset := len(c.ipTargets)
	for i, target := range c.domainTargets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			results[offset+i] = c.probeHTTP(ctx, target)
		}(i, target)
	}
	wg.Wait()

	return domain.Verdict(results)
}

// Gateway probes the router's web UI host with a plain TCP dial (design §5)
// so the recovery loop can distinguish "router still rebooting" from
// "router back up, internet still down".
func (c *Checker) Gateway(ctx context.Context) domain.TargetResult {
	return c.probeTCP(ctx, c.gatewayTarget, domain.CheckKindGateway)
}

// probeHTTP GETs target within c.timeout and accepts any 2xx status (204 for
// generate_204, 200 for cdn-cgi/trace) as success; anything else — including
// a redirect, which CheckRedirect stops the client from following — is a
// failure with the response status recorded in Error.
func (c *Checker) probeHTTP(ctx context.Context, target string) domain.TargetResult {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return domain.TargetResult{Target: target, Kind: domain.CheckKindDomain, OK: false, Latency: time.Since(start), Error: err.Error()}
	}

	resp, err := c.httpClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return domain.TargetResult{Target: target, Kind: domain.CheckKindDomain, OK: false, Latency: latency, Error: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.TargetResult{
			Target:  target,
			Kind:    domain.CheckKindDomain,
			OK:      false,
			Latency: latency,
			Error:   fmt.Sprintf("unexpected status %s", resp.Status),
		}
	}

	return domain.TargetResult{Target: target, Kind: domain.CheckKindDomain, OK: true, Latency: latency}
}

// probeTCP dials target (host:port) over TCP within c.timeout, recording the
// elapsed latency regardless of outcome so a failed or timed-out probe still
// contributes a latency to checks.latency_ms.
func (c *Checker) probeTCP(ctx context.Context, target, kind string) domain.TargetResult {
	dialer := net.Dialer{Timeout: c.timeout}

	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", target)
	latency := time.Since(start)
	if err != nil {
		return domain.TargetResult{Target: target, Kind: kind, OK: false, Latency: latency, Error: err.Error()}
	}
	defer conn.Close()

	return domain.TargetResult{Target: target, Kind: kind, OK: true, Latency: latency}
}

// hostPortFromURL extracts a dialable host:port from a base URL like
// "http://192.168.1.1", defaulting the port from the URL scheme (80/443)
// when the URL doesn't specify one explicitly.
func hostPortFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("no host in %q", rawURL)
	}

	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}

	return net.JoinHostPort(u.Hostname(), port), nil
}
