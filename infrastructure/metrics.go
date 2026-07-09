package infrastructure

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// netIface is one /proc/net/dev interface's cumulative byte counters, an
// intermediate parse result consumed only by the linux collector.
type netIface struct {
	Name    string
	RxBytes uint64
	TxBytes uint64
}

// darwinAvailableBytes approximates memory available to new allocations as
// (free + inactive + speculative) pages, since macOS has no single
// authoritative "available bytes" counter the way linux's /proc/meminfo
// MemAvailable is — this is documented as an approximation, matching
// roughly what Activity Monitor calls "available".
func darwinAvailableBytes(pageSize uint64, pages map[string]uint64) uint64 {
	return (pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"]) * pageSize
}

// diskSamples builds the five disk.* samples (total/free/avail/used/
// used_ratio) shared byte-for-byte between the linux and darwin collectors;
// each keeps its own platform-specific syscall.Statfs field extraction
// (Bsize's width differs: int64 on linux, uint32 on darwin) and calls this
// once bytes are resolved.
func diskSamples(mount string, total, free, avail, used uint64, usedRatio float64) []domain.MetricSample {
	// the root mount would otherwise name samples "/ total" etc.; drop the "/ "
	// prefix for "/" only, so root reads "total"/"used"/… while any other mount
	// keeps its prefix ("/data total").
	prefix := mount + " "
	if mount == "/" {
		prefix = ""
	}
	return []domain.MetricSample{
		{Collector: domain.CollectorDisk, Name: prefix + "total", Value: float64(total), Unit: "bytes", OK: true},
		{Collector: domain.CollectorDisk, Name: prefix + "free", Value: float64(free), Unit: "bytes", OK: true},
		{Collector: domain.CollectorDisk, Name: prefix + "avail", Value: float64(avail), Unit: "bytes", OK: true},
		{Collector: domain.CollectorDisk, Name: prefix + "used", Value: float64(used), Unit: "bytes", OK: true},
		{Collector: domain.CollectorDisk, Name: prefix + "used_ratio", Value: usedRatio, Unit: "ratio", OK: true},
	}
}

// diskUsage computes used bytes and the used ratio from a filesystem's
// total/free byte counts (already converted from blocks by the caller,
// since syscall.Statfs's block-count field types differ per GOOS).
func diskUsage(total, free uint64) (used uint64, usedRatio float64) {
	if total == 0 {
		return 0, 0
	}
	used = total - free
	return used, float64(used) / float64(total)
}

// hostInfo identifies the running host: os.Hostname falling back to
// "unknown" rather than failing the whole snapshot over a hostname lookup
// error. OS is the platform's human-readable description (osDescription,
// build-tagged); Arch is runtime.GOARCH.
func hostInfo() domain.HostInfo {
	name, err := os.Hostname()
	if err != nil {
		name = "unknown"
	}
	return domain.HostInfo{Hostname: name, OS: osDescription(), Arch: runtime.GOARCH}
}

// parseOSRelease extracts a human-readable OS name from os-release file
// content: the value of PRETTY_NAME with its surrounding shell quotes
// stripped (`PRETTY_NAME="Ubuntu 26.04 LTS"` -> "Ubuntu 26.04 LTS"), or ""
// when the key is absent — the linux osDescription then falls back to
// runtime.GOOS.
func parseOSRelease(content string) string {
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "PRETTY_NAME" {
			continue
		}
		return strings.Trim(strings.TrimSpace(val), `"'`)
	}
	return ""
}

// parseSwVers builds a human-readable macOS name from `sw_vers` output,
// joining ProductName and ProductVersion ("macOS 15.2"), or "" when
// ProductName is absent — the darwin osDescription then falls back to
// runtime.GOOS. A ProductVersion with no ProductName still yields "".
func parseSwVers(content string) string {
	var name, version string
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ProductName":
			name = strings.TrimSpace(val)
		case "ProductVersion":
			version = strings.TrimSpace(val)
		}
	}
	switch {
	case name != "" && version != "":
		return name + " " + version
	case name != "":
		return name
	default:
		return ""
	}
}

// parseHwmonFan parses a hwmon fanN_input RPM integer string ("1200" ->
// 1200 RPM). Unlike temperature, no unit conversion applies: the kernel
// already reports whole RPM.
func parseHwmonFan(raw string) (float64, error) {
	rpm, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("parse hwmon fan %q: %w", raw, err)
	}
	return float64(rpm), nil
}

// parseLoadavg parses /proc/loadavg's first three fields (1/5/15-minute
// load averages); the remaining "runnable/total processes" and "last pid"
// fields are ignored.
func parseLoadavg(raw string) (l1, l5, l15 float64, err error) {
	l1, l5, l15, err = parseLoadTriple(strings.Fields(raw))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse loadavg %q: %w", raw, err)
	}
	return l1, l5, l15, nil
}

// parseLoadTriple parses fields[0:3] as the 1/5/15-minute load-average
// triple shared by parseLoadavg and parseSysctlLoadavg, once each caller has
// stripped its own format's wrapper (loadavg has none; sysctl's is "{ }").
func parseLoadTriple(fields []string) (l1, l5, l15 float64, err error) {
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("want at least 3 fields")
	}

	vals := make([]float64, 3)
	for i := range vals {
		vals[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("field %d: %w", i, err)
		}
	}
	return vals[0], vals[1], vals[2], nil
}

// parseMeminfo parses /proc/meminfo's MemTotal/MemFree/MemAvailable lines
// into bytes (the file reports kB, so each is multiplied by 1024).
// MemAvailable, not MemFree, is the "available for new allocations" figure
// (it accounts for reclaimable caches); both are recorded as separate
// samples by the caller.
func parseMeminfo(raw string) (total, free, available uint64, err error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(raw, "\n") {
		key, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "MemTotal" && key != "MemFree" && key != "MemAvailable" {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, 0, 0, fmt.Errorf("parse meminfo: %s has no value", key)
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("parse meminfo: %s: %w", key, err)
		}
		values[key] = kb * 1024
	}

	var ok bool
	if total, ok = values["MemTotal"]; !ok {
		return 0, 0, 0, fmt.Errorf("parse meminfo: MemTotal not found")
	}
	if free, ok = values["MemFree"]; !ok {
		return 0, 0, 0, fmt.Errorf("parse meminfo: MemFree not found")
	}
	if available, ok = values["MemAvailable"]; !ok {
		return 0, 0, 0, fmt.Errorf("parse meminfo: MemAvailable not found")
	}
	return total, free, available, nil
}

// parseNetDev parses /proc/net/dev's two-line header followed by one line
// per interface, returning rx/tx cumulative byte counters. It excludes the
// loopback interface (never a meaningful telemetry sample) and skips any
// line it cannot parse instead of failing the whole file: one malformed or
// unexpected interface line must not zero out every other interface's
// metrics, matching every other collector's best-effort contract.
func parseNetDev(raw string) []netIface {
	var out []netIface
	for i, line := range strings.Split(raw, "\n") {
		if i < 2 { // "Inter-|   Receive ..." and "  face |bytes ..." headers
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rxBytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		txBytes, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, netIface{Name: name, RxBytes: rxBytes, TxBytes: txBytes})
	}
	return out
}

// parseSysctlBoottime parses macOS's `sysctl -n kern.boottime` output
// ("{ sec = 1712345678, usec = 0 } Wed Apr ... 2024") into its epoch
// seconds field. It matches the "sec" key by exact, trimmed comparison
// after splitting on ",", rather than a bare strings.Index(raw, "sec"),
// which would also match inside "usec" and could silently misread that
// field as the boot time.
func parseSysctlBoottime(raw string) (int64, error) {
	braceEnd := strings.IndexByte(raw, '}')
	if braceEnd < 0 {
		braceEnd = len(raw)
	}

	for _, field := range strings.Split(raw[:braceEnd], ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if strings.Trim(key, "{} \t") != "sec" {
			continue
		}
		sec, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse sysctl boottime %q: %w", raw, err)
		}
		return sec, nil
	}
	return 0, fmt.Errorf("parse sysctl boottime %q: %q not found", raw, "sec")
}

// parseSysctlLoadavg parses macOS's `sysctl -n vm.loadavg` output
// ("{ 0.52 0.58 0.59 }") into its three load averages.
func parseSysctlLoadavg(raw string) (l1, l5, l15 float64, err error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "{"), "}")
	l1, l5, l15, err = parseLoadTriple(strings.Fields(trimmed))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse sysctl loadavg %q: %w", raw, err)
	}
	return l1, l5, l15, nil
}

// parseSysctlUint parses a bare unsigned integer sysctl value (e.g.
// `sysctl -n hw.memsize`).
func parseSysctlUint(raw string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sysctl uint %q: %w", raw, err)
	}
	return v, nil
}

// parseThermalTemp parses a millidegree Celsius integer string, as sysfs
// serves it identically in both thermal_zone*/temp and hwmon*/tempN_input
// ("52350" -> 52.35).
func parseThermalTemp(raw string) (float64, error) {
	milli, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("parse thermal temp %q: %w", raw, err)
	}
	return float64(milli) / 1000.0, nil
}

// parseUptime parses /proc/uptime's first field (seconds since boot); the
// second field (cumulative idle time) is not collected.
func parseUptime(raw string) (float64, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0, fmt.Errorf("parse uptime %q: no fields", raw)
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime %q: %w", raw, err)
	}
	return seconds, nil
}

// parseVmStat parses macOS's `vm_stat` output into its page size (from the
// header) and a name->count map of every "Pages ...:" line (trailing
// periods stripped).
func parseVmStat(raw string) (pageSize uint64, pages map[string]uint64, err error) {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return 0, nil, fmt.Errorf("parse vm_stat: empty output")
	}

	header := lines[0]
	idx := strings.Index(header, "page size of")
	if idx < 0 {
		return 0, nil, fmt.Errorf("parse vm_stat header %q: \"page size of\" not found", header)
	}
	fields := strings.Fields(header[idx+len("page size of"):])
	if len(fields) == 0 {
		return 0, nil, fmt.Errorf("parse vm_stat header %q: no page size value", header)
	}
	pageSize, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("parse vm_stat header %q: %w", header, err)
	}

	pages = map[string]uint64{}
	for _, line := range lines[1:] {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value := strings.TrimSuffix(strings.TrimSpace(rest), ".")
		count, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("parse vm_stat %q: %w", name, err)
		}
		pages[name] = count
	}

	return pageSize, pages, nil
}

// unavailable builds the uniform "not measured" sample every collector
// falls back to when a collector is unsupported, absent, or fails to read:
// OK is false, Value stays zero (persisted as SQL NULL), and reason
// explains why.
func unavailable(c domain.Collector, name, reason string) domain.MetricSample {
	return domain.MetricSample{Collector: c, Name: name, OK: false, Error: reason}
}

// uptimeSeconds returns the host uptime in seconds, computed as now minus
// the kern.boottime epoch macOS's sysctl reports — unlike linux's
// /proc/uptime, darwin has no direct "seconds since boot" reading.
func uptimeSeconds(boottimeEpoch int64, now time.Time) float64 {
	return now.Sub(time.Unix(boottimeEpoch, 0)).Seconds()
}
