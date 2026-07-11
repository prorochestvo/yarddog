//go:build darwin

package infrastructure

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/prorochestvo/yarddog/services"
)

// NewPingCollector builds the darwin ping collector (issue #2): `ping -c
// <count> -t <timeoutSecs> <host>` via os/exec, no shell — darwin's ping has
// no -w, so -t bounds the whole run instead.
func NewPingCollector(hosts []string, count int, timeout time.Duration) services.PingCollector {
	return &pingCollector{hosts: hosts, count: count, timeout: timeout, ping: runPing}
}

var _ services.PingCollector = (*pingCollector)(nil)

// pingBinPath pins ping's canonical darwin location (mirrors sysctlPath/
// vmStatPath in metrics_darwin.go).
const pingBinPath = "/sbin/ping"

// runPing runs one `ping -c <count> -t <timeoutSecs> <host>` (-t is the
// overall timeout in whole seconds, floored to 1) and returns its stdout
// regardless of exit code, mirroring ping_linux.go's runPing: a 100%-loss
// ping exits non-zero but still prints a parseable summary, so only a truly
// failed exec (e.g. the binary is absent) yields an empty string alongside
// its error.
func runPing(ctx context.Context, host string, count int, timeout time.Duration) (string, error) {
	timeoutSecs := int(timeout.Round(time.Second).Seconds())
	if timeoutSecs < 1 {
		timeoutSecs = 1
	}

	cmd := exec.CommandContext(ctx, pingBinPath, "-c", strconv.Itoa(count), "-t", strconv.Itoa(timeoutSecs), host)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/sbin", "LC_ALL=C"}

	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("run ping %s: %w", host, err)
	}
	return string(out), nil
}
