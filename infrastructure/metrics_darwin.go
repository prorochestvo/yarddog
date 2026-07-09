//go:build darwin

package infrastructure

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

// NewMetricsCollector builds the darwin host telemetry collector:
// syscall.Statfs for disk, os/exec of sysctl/vm_stat (no sudo) for
// cpu/memory/uptime (plans/003-host-telemetry.md). Temperature, fans, and
// network are recorded unavailable: SMC needs cgo and procfs is
// linux-only, and CGO_ENABLED=0 on every target including darwin is
// non-negotiable — do not reintroduce either by any route.
func NewMetricsCollector(settings domain.MetricsSettings, diskMount string) services.MetricsCollector {
	return &darwinCollector{settings: settings, diskMount: diskMount}
}

// darwinCollector is the darwin services.MetricsCollector implementation.
type darwinCollector struct {
	settings  domain.MetricsSettings
	diskMount string
}

var _ services.MetricsCollector = (*darwinCollector)(nil)

// Collect takes one telemetry snapshot, running only the enabled
// collectors. It never returns an error: every exec/Statfs failure becomes
// an unavailable domain.MetricSample instead.
func (c *darwinCollector) Collect(ctx context.Context) domain.HostMetrics {
	m := domain.HostMetrics{Host: hostInfo()}

	if c.settings.Enabled(domain.CollectorTemperature) {
		m.Samples = append(m.Samples, unavailable(domain.CollectorTemperature, "temperature", "unsupported on darwin (SMC needs cgo)"))
	}
	if c.settings.Enabled(domain.CollectorFans) {
		m.Samples = append(m.Samples, unavailable(domain.CollectorFans, "fans", "unsupported on darwin (SMC needs cgo)"))
	}
	if c.settings.Enabled(domain.CollectorNetwork) {
		m.Samples = append(m.Samples, unavailable(domain.CollectorNetwork, "network", "unsupported on darwin (procfs is linux-only)"))
	}
	if c.settings.Enabled(domain.CollectorCPU) {
		m.Samples = append(m.Samples, c.loadavg(ctx)...)
	}
	if c.settings.Enabled(domain.CollectorMemory) {
		m.Samples = append(m.Samples, c.memory(ctx)...)
	}
	if c.settings.Enabled(domain.CollectorDisk) {
		m.Samples = append(m.Samples, c.disk()...)
	}
	if c.settings.Enabled(domain.CollectorUptime) {
		m.Samples = append(m.Samples, c.uptime(ctx)...)
	}

	return m
}

// disk reports usage of diskMount via syscall.Statfs.
func (c *darwinCollector) disk() []domain.MetricSample {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(c.diskMount, &stat); err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorDisk, c.diskMount, err.Error())}
	}

	bsize := uint64(stat.Bsize) // uint32 on darwin
	total := stat.Blocks * bsize
	free := stat.Bfree * bsize
	avail := stat.Bavail * bsize
	used, usedRatio := diskUsage(total, free)

	return diskSamples(c.diskMount, total, free, avail, used, usedRatio)
}

// loadavg runs `sysctl -n vm.loadavg` for the 1/5/15-minute load averages.
func (c *darwinCollector) loadavg(ctx context.Context) []domain.MetricSample {
	raw, err := runCmd(ctx, sysctlPath, "-n", "vm.loadavg")
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorCPU, "cpu", err.Error())}
	}
	l1, l5, l15, err := parseSysctlLoadavg(raw)
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorCPU, "cpu", err.Error())}
	}
	return []domain.MetricSample{
		{Collector: domain.CollectorCPU, Name: "load1", Value: l1, Unit: "load", OK: true},
		{Collector: domain.CollectorCPU, Name: "load5", Value: l5, Unit: "load", OK: true},
		{Collector: domain.CollectorCPU, Name: "load15", Value: l15, Unit: "load", OK: true},
	}
}

// memory runs `sysctl -n hw.memsize` for total bytes and `vm_stat` for
// free/available bytes (derived from page counts).
func (c *darwinCollector) memory(ctx context.Context) []domain.MetricSample {
	memsizeRaw, err := runCmd(ctx, sysctlPath, "-n", "hw.memsize")
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorMemory, "memory", err.Error())}
	}
	total, err := parseSysctlUint(memsizeRaw)
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorMemory, "memory", err.Error())}
	}

	vmStatRaw, err := runCmd(ctx, vmStatPath)
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorMemory, "memory", err.Error())}
	}
	pageSize, pages, err := parseVmStat(vmStatRaw)
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorMemory, "memory", err.Error())}
	}

	free := pages["Pages free"] * pageSize
	available := darwinAvailableBytes(pageSize, pages)

	return []domain.MetricSample{
		{Collector: domain.CollectorMemory, Name: "total", Value: float64(total), Unit: "bytes", OK: true},
		{Collector: domain.CollectorMemory, Name: "free", Value: float64(free), Unit: "bytes", OK: true},
		{Collector: domain.CollectorMemory, Name: "available", Value: float64(available), Unit: "bytes", OK: true},
	}
}

// uptime runs `sysctl -n kern.boottime` and computes uptime as now minus
// that boot epoch (darwin has no direct "seconds since boot" reading).
func (c *darwinCollector) uptime(ctx context.Context) []domain.MetricSample {
	raw, err := runCmd(ctx, sysctlPath, "-n", "kern.boottime")
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorUptime, "uptime", err.Error())}
	}
	boottime, err := parseSysctlBoottime(raw)
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorUptime, "uptime", err.Error())}
	}
	return []domain.MetricSample{
		{Collector: domain.CollectorUptime, Name: "uptime", Value: uptimeSeconds(boottime, time.Now()), Unit: "seconds", OK: true},
	}
}

// sysctlPath, vmStatPath, and swVersPath pin the canonical, SIP-protected
// macOS locations of the executables runCmd invokes, so a caller can never be
// tricked by a modified PATH into running some other "sysctl"/"vm_stat"/
// "sw_vers" ahead of the real one.
const (
	sysctlPath = "/usr/sbin/sysctl"
	vmStatPath = "/usr/bin/vm_stat"
	swVersPath = "/usr/bin/sw_vers"
)

// osDescription returns a human-readable OS name via sw_vers ("macOS
// <version>"), falling back to runtime.GOOS when sw_vers is unavailable. It
// passes context.Background bounded by runCmd's own timeout, since hostInfo —
// its only caller — carries no context; parseSwVers carries the tests.
func osDescription() string {
	raw, err := runCmd(context.Background(), swVersPath)
	if err == nil {
		if name := parseSwVers(raw); name != "" {
			return name
		}
	}
	return runtime.GOOS
}

// runCmd executes name (an absolute path — sysctlPath/vmStatPath) with args
// (no shell, no sudo) bounded by a short timeout so a hung sysctl/vm_stat
// cannot stall the run (plans/003-host-telemetry.md). Env is pinned to a
// minimal PATH instead of inherited, so the child process never sees
// yarddog's own environment — which, per config.LoadConfig, may carry
// ROUTER_PASS/TELEGRAMBOT_DSN as real (not just file-based) env vars.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin"}

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return string(out), nil
}
