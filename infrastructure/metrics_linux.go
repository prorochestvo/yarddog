//go:build linux

package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

// NewMetricsCollector builds the linux host telemetry collector: sysfs for
// temperature/fans, procfs for cpu/memory/uptime/network, syscall.Statfs
// for disk (plans/003-host-telemetry.md). It reads under "/" in production;
// tests construct a linuxCollector directly with root pointed at a fixture
// tree.
func NewMetricsCollector(settings domain.MetricsSettings, diskMount string) services.MetricsCollector {
	return &linuxCollector{settings: settings, diskMount: diskMount}
}

// linuxCollector is the linux services.MetricsCollector implementation.
// root is the injectable filesystem root: empty in production (paths join
// against "/"), a fixture directory in tests.
type linuxCollector struct {
	root      string
	settings  domain.MetricsSettings
	diskMount string
}

var _ services.MetricsCollector = (*linuxCollector)(nil)

// Collect takes one telemetry snapshot, running only the enabled
// collectors (METRICS_<COLLECTOR> toggles). It never returns an error:
// every read/parse failure becomes an unavailable domain.MetricSample
// instead. It checks ctx between collectors so an orchestrator-side timeout
// (services.runner.collectMetrics) is honored best-effort: a blocking
// os.ReadFile/syscall.Statfs cannot itself be interrupted mid-read, so the
// caller's own goroutine+select remains what actually bounds a wedged
// sensor — this only skips collectors not yet started once the deadline has
// passed.
func (c *linuxCollector) Collect(ctx context.Context) domain.HostMetrics {
	m := domain.HostMetrics{Host: hostInfo()}

	collect := func(enabled bool, samples func() []domain.MetricSample) {
		if ctx.Err() != nil || !enabled {
			return
		}
		m.Samples = append(m.Samples, samples()...)
	}

	collect(c.settings.Enabled(domain.CollectorTemperature), c.temperature)
	collect(c.settings.Enabled(domain.CollectorFans), c.fans)
	collect(c.settings.Enabled(domain.CollectorCPU), c.loadavg)
	collect(c.settings.Enabled(domain.CollectorMemory), c.memory)
	collect(c.settings.Enabled(domain.CollectorDisk), c.disk)
	collect(c.settings.Enabled(domain.CollectorUptime), c.uptime)
	collect(c.settings.Enabled(domain.CollectorNetwork), c.network)

	return m
}

// disk reports usage of diskMount via syscall.Statfs, which is not
// root-injectable (it always hits the real filesystem — the pure byte math
// is what testdata fixtures exercise instead, see diskUsage in metrics.go).
func (c *linuxCollector) disk() []domain.MetricSample {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(c.diskMount, &stat); err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorDisk, c.diskMount, err.Error())}
	}

	bsize := uint64(stat.Bsize) // int64 on linux
	total := stat.Blocks * bsize
	free := stat.Bfree * bsize
	avail := stat.Bavail * bsize
	used, usedRatio := diskUsage(total, free)

	return diskSamples(c.diskMount, total, free, avail, used, usedRatio)
}

// fans scans every hwmon's fan*_input files. No fan*_input anywhere is the
// common case on hosts with no fan sensor (e.g. the pi5) and is reported as
// one unavailable sample, not silence.
func (c *linuxCollector) fans() []domain.MetricSample {
	hwmons, err := filepath.Glob(c.path("sys/class/hwmon/hwmon*"))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorFans, "fans", err.Error())}
	}

	var samples []domain.MetricSample
	for _, hwmon := range hwmons {
		nameRaw, err := os.ReadFile(filepath.Join(hwmon, "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(nameRaw))

		inputs, err := filepath.Glob(filepath.Join(hwmon, "fan*_input"))
		if err != nil {
			continue
		}
		multi := len(inputs) > 1
		for _, input := range inputs {
			raw, err := os.ReadFile(input)
			if err != nil {
				continue
			}
			rpm, err := parseHwmonFan(string(raw))
			if err != nil {
				continue
			}
			sampleName := name
			if multi {
				sampleName = name + " " + strings.TrimSuffix(filepath.Base(input), "_input")
			}
			samples = append(samples, domain.MetricSample{Collector: domain.CollectorFans, Name: sampleName, Value: rpm, Unit: "rpm", OK: true})
		}
	}

	if len(samples) == 0 {
		return []domain.MetricSample{unavailable(domain.CollectorFans, "fans", "no fan sensors present")}
	}
	return samples
}

// loadavg reads /proc/loadavg for the 1/5/15-minute load averages.
func (c *linuxCollector) loadavg() []domain.MetricSample {
	raw, err := os.ReadFile(c.path("proc/loadavg"))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorCPU, "cpu", err.Error())}
	}
	l1, l5, l15, err := parseLoadavg(string(raw))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorCPU, "cpu", err.Error())}
	}
	return []domain.MetricSample{
		{Collector: domain.CollectorCPU, Name: "load1", Value: l1, Unit: "load", OK: true},
		{Collector: domain.CollectorCPU, Name: "load5", Value: l5, Unit: "load", OK: true},
		{Collector: domain.CollectorCPU, Name: "load15", Value: l15, Unit: "load", OK: true},
	}
}

// memory reads /proc/meminfo for total/free/available bytes.
func (c *linuxCollector) memory() []domain.MetricSample {
	raw, err := os.ReadFile(c.path("proc/meminfo"))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorMemory, "memory", err.Error())}
	}
	total, free, available, err := parseMeminfo(string(raw))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorMemory, "memory", err.Error())}
	}
	return []domain.MetricSample{
		{Collector: domain.CollectorMemory, Name: "total", Value: float64(total), Unit: "bytes", OK: true},
		{Collector: domain.CollectorMemory, Name: "free", Value: float64(free), Unit: "bytes", OK: true},
		{Collector: domain.CollectorMemory, Name: "available", Value: float64(available), Unit: "bytes", OK: true},
	}
}

// network reads /proc/net/dev for per-interface rx/tx byte counters,
// excluding loopback.
func (c *linuxCollector) network() []domain.MetricSample {
	raw, err := os.ReadFile(c.path("proc/net/dev"))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorNetwork, "network", err.Error())}
	}
	ifaces := parseNetDev(string(raw))

	samples := make([]domain.MetricSample, 0, len(ifaces)*2)
	for _, iface := range ifaces {
		samples = append(samples,
			domain.MetricSample{Collector: domain.CollectorNetwork, Name: iface.Name + " rx_bytes", Value: float64(iface.RxBytes), Unit: "bytes", OK: true},
			domain.MetricSample{Collector: domain.CollectorNetwork, Name: iface.Name + " tx_bytes", Value: float64(iface.TxBytes), Unit: "bytes", OK: true},
		)
	}
	return samples
}

// path joins rel onto c.root, defaulting root to "/" in production; tests
// point root at a fixture tree so the same read code exercises captured
// sysfs/procfs samples.
func (c *linuxCollector) path(rel string) string {
	root := c.root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, rel)
}

// temperature scans thermal_zone*/type+temp and hwmon*/name+temp*_input.
// The same physical sensor can appear under both paths (the pi5's
// cpu-thermal thermal_zone and cpu_thermal hwmon are the same sensor) — no
// dedup is attempted, since that would need name-matching heuristics; the
// collector stays dumb and records both.
func (c *linuxCollector) temperature() []domain.MetricSample {
	var samples []domain.MetricSample

	zones, err := filepath.Glob(c.path("sys/class/thermal/thermal_zone*"))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorTemperature, "temperature", err.Error())}
	}
	for _, zone := range zones {
		typeRaw, err := os.ReadFile(filepath.Join(zone, "type"))
		if err != nil {
			continue
		}
		tempRaw, err := os.ReadFile(filepath.Join(zone, "temp"))
		if err != nil {
			continue
		}
		celsius, err := parseThermalTemp(string(tempRaw))
		if err != nil {
			continue
		}
		samples = append(samples, domain.MetricSample{Collector: domain.CollectorTemperature, Name: strings.TrimSpace(string(typeRaw)), Value: celsius, Unit: "celsius", OK: true})
	}

	hwmons, err := filepath.Glob(c.path("sys/class/hwmon/hwmon*"))
	if err != nil {
		return append(samples, unavailable(domain.CollectorTemperature, "temperature", err.Error()))
	}
	for _, hwmon := range hwmons {
		nameRaw, err := os.ReadFile(filepath.Join(hwmon, "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(nameRaw))

		inputs, err := filepath.Glob(filepath.Join(hwmon, "temp*_input"))
		if err != nil {
			continue
		}
		multi := len(inputs) > 1
		for _, input := range inputs {
			raw, err := os.ReadFile(input)
			if err != nil {
				continue
			}
			celsius, err := parseThermalTemp(string(raw))
			if err != nil {
				continue
			}
			sampleName := name
			if multi {
				sampleName = name + " " + strings.TrimSuffix(filepath.Base(input), "_input")
			}
			samples = append(samples, domain.MetricSample{Collector: domain.CollectorTemperature, Name: sampleName, Value: celsius, Unit: "celsius", OK: true})
		}
	}

	if len(samples) == 0 {
		return []domain.MetricSample{unavailable(domain.CollectorTemperature, "temperature", "no temperature sensors present")}
	}
	return samples
}

// uptime reads /proc/uptime for the host's uptime in seconds.
func (c *linuxCollector) uptime() []domain.MetricSample {
	raw, err := os.ReadFile(c.path("proc/uptime"))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorUptime, "uptime", err.Error())}
	}
	seconds, err := parseUptime(string(raw))
	if err != nil {
		return []domain.MetricSample{unavailable(domain.CollectorUptime, "uptime", err.Error())}
	}
	return []domain.MetricSample{
		{Collector: domain.CollectorUptime, Name: "uptime", Value: seconds, Unit: "seconds", OK: true},
	}
}

// osDescription returns a human-readable OS name from /etc/os-release
// (PRETTY_NAME, e.g. "Ubuntu 26.04 LTS"), falling back to runtime.GOOS when
// the file is unreadable or carries no PRETTY_NAME. It reads the real
// /etc/os-release rather than c.path because host identity — like os.Hostname
// in hostInfo — is not fixture-injectable; parseOSRelease carries the tests.
func osDescription() string {
	b, err := os.ReadFile("/etc/os-release")
	if err == nil {
		if name := parseOSRelease(string(b)); name != "" {
			return name
		}
	}
	return runtime.GOOS
}
