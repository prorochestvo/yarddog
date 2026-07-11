package infrastructure

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// pingCollector is the platform-shared services.PingCollector implementation
// (issue #2): Collect's concurrency and per-host fallback logic is identical
// on linux and darwin — only the exact ping invocation (ping_linux.go's and
// ping_darwin.go's NewPingCollector/runPing) differs. ping is injected so
// tests exercise Collect without ever exec'ing a real binary.
type pingCollector struct {
	hosts   []string
	count   int
	timeout time.Duration
	ping    func(ctx context.Context, host string, count int, timeout time.Duration) (string, error)
}

// Collect runs one round of ping probes concurrently — one goroutine per
// host, each writing to its own fixed slice index so results come back in
// c.hosts order with no mutex needed (mirrors infrastructure.Checker.Check).
// It never returns an error: a failed exec or an unparseable summary becomes
// an unavailable domain.PingResult instead.
func (c *pingCollector) Collect(ctx context.Context) []domain.PingResult {
	results := make([]domain.PingResult, len(c.hosts))

	var wg sync.WaitGroup
	for i, host := range c.hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			results[i] = c.probe(ctx, host)
		}(i, host)
	}
	wg.Wait()

	return results
}

// probe runs one host's ping and turns its outcome into a domain.PingResult.
// A parseable summary is used even when the underlying exec itself reported
// an error: 100% packet loss exits non-zero but still prints a parseable
// summary (see ping_linux.go/ping_darwin.go's runPing). Only a summary that
// fails to parse at all — a DNS failure, an absent ping binary, or a
// cancelled context — falls back to an unavailable result, in which case the
// exec error (if any) is the more useful message than the parse error.
func (c *pingCollector) probe(ctx context.Context, host string) domain.PingResult {
	out, execErr := c.ping(ctx, host, c.count, c.timeout)

	sent, received, avgMS, parseErr := parsePingSummary(out)
	if parseErr != nil {
		errMsg := parseErr.Error()
		if execErr != nil {
			errMsg = execErr.Error()
		}
		return domain.PingResult{Host: host, Sent: c.count, Received: 0, OK: false, Error: errMsg}
	}

	return domain.PingResult{Host: host, Sent: sent, Received: received, AvgMS: avgMS, OK: received > 0}
}

// leadingInt parses the first whitespace-delimited field of s as an int
// (" 2 received" -> 2, "2 packets transmitted" -> 2, " 2 packets received"
// -> 2): both the linux and macOS ping summary dialects put the sent/
// received count as the first field of their comma-separated segment, so one
// field-based parse covers both.
func leadingInt(s string) (int, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("no fields in %q", s)
	}
	return strconv.Atoi(fields[0])
}

// parsePingSummary parses ping's textual summary, handling both
// linux/iputils ("N packets transmitted, M received, ...") and macOS/BSD
// ("N packets transmitted, M packets received, ...") wording via leadingInt.
// received==0 (100% packet loss) is itself a valid, errorless parse — the
// rtt/round-trip line is simply absent, leaving avgMS at 0 via parseRTTAvg.
// An error is returned only when no "packets transmitted" line exists at
// all: a DNS failure or a missing ping binary never produces a summary to
// parse in the first place.
func parsePingSummary(raw string) (sent, received int, avgMS float64, err error) {
	var summaryLine string
	for _, line := range strings.Split(raw, "\n") {
		if strings.Contains(line, "packets transmitted") {
			summaryLine = line
			break
		}
	}
	if summaryLine == "" {
		return 0, 0, 0, fmt.Errorf("parse ping summary: no %q line in output", "packets transmitted")
	}

	segments := strings.Split(summaryLine, ",")
	if len(segments) < 2 {
		return 0, 0, 0, fmt.Errorf("parse ping summary %q: expected at least 2 comma-separated fields", summaryLine)
	}

	if sent, err = leadingInt(segments[0]); err != nil {
		return 0, 0, 0, fmt.Errorf("parse ping sent count %q: %w", segments[0], err)
	}
	if received, err = leadingInt(segments[1]); err != nil {
		return 0, 0, 0, fmt.Errorf("parse ping received count %q: %w", segments[1], err)
	}

	return sent, received, parseRTTAvg(raw), nil
}

// parseRTTAvg scans raw for the rtt ("rtt min/avg/max/mdev = ... ms", linux)
// or round-trip ("round-trip min/avg/max/stddev = ... ms", macOS) summary
// line and returns its avg field (the second "/"-separated value on both
// dialects), or 0 if no such line is present — a 100%-loss summary has none,
// which is not itself a parse failure (see parsePingSummary).
func parseRTTAvg(raw string) float64 {
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "rtt ") && !strings.HasPrefix(trimmed, "round-trip ") {
			continue
		}

		_, rhs, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		rhs = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rhs), "ms"))

		parts := strings.Split(rhs, "/")
		if len(parts) < 2 {
			continue
		}
		if avg, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
			return avg
		}
	}
	return 0
}
