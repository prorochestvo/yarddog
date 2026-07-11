//go:build linux

package infrastructure

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/prorochestvo/yarddog/services"
)

// NewPingCollector builds the linux ping collector (issue #2): `ping -c
// <count> -w <deadlineSecs> <host>` via os/exec, no shell.
func NewPingCollector(hosts []string, count int, timeout time.Duration) services.PingCollector {
	return &pingCollector{hosts: hosts, count: count, timeout: timeout, ping: runPing}
}

var _ services.PingCollector = (*pingCollector)(nil)

// pingBinPath pins ping's canonical linux location, so a caller can never be
// tricked by a modified PATH into running some other "ping" ahead of the
// real one (mirrors sysctlPath/vmStatPath in metrics_darwin.go).
const pingBinPath = "/usr/bin/ping"

// runPing runs one `ping -c <count> -w <deadlineSecs> <host>` (-w is the
// overall deadline in whole seconds, floored to 1) and returns its stdout
// regardless of exit code: a 100%-loss ping exits non-zero but still prints
// a parseable summary, so only a truly failed exec (e.g. the binary is
// absent) yields an empty string alongside its error.
func runPing(ctx context.Context, host string, count int, timeout time.Duration) (string, error) {
	deadlineSecs := int(timeout.Round(time.Second).Seconds())
	if deadlineSecs < 1 {
		deadlineSecs = 1
	}

	cmd := exec.CommandContext(ctx, pingBinPath, "-c", strconv.Itoa(count), "-w", strconv.Itoa(deadlineSecs), host)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}

	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("run ping %s: %w", host, err)
	}
	return string(out), nil
}
