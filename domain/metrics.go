package domain

// CollectorTemperature, CollectorFans, CollectorCPU, CollectorMemory,
// CollectorDisk, CollectorUptime, and CollectorNetwork are the values
// allowed in metrics.collector (plans/003-host-telemetry.md). They are
// persisted verbatim — do not rename a value without a data migration (see
// TestPersistedConstants).
const (
	CollectorTemperature Collector = "temperature"
	CollectorFans        Collector = "fans"
	CollectorCPU         Collector = "cpu"
	CollectorMemory      Collector = "memory"
	CollectorDisk        Collector = "disk"
	CollectorUptime      Collector = "uptime"
	CollectorNetwork     Collector = "network"
)

// Collector names one of the host telemetry collectors (plans/003-host-telemetry.md).
type Collector string

// MetricSample is one measured or unavailable host metric, shaped to map
// directly onto a metrics row once the caller supplies RunID. Value is
// meaningful only when OK is true; an unavailable, unsupported, or failed
// sample sets OK false, leaves Value at its zero value (persisted as SQL
// NULL), and explains itself in Error.
type MetricSample struct {
	Collector Collector
	Name      string // e.g. "cpu-thermal", "load1", "/ used_ratio", "eth0 rx_bytes"
	Value     float64
	Unit      string // "celsius", "rpm", "bytes", "ratio", "load", "seconds", ""
	OK        bool
	Error     string
}

// HostInfo identifies the host a telemetry snapshot was taken on. OS is a
// human-readable description ("Ubuntu 26.04 LTS", "macOS 15.2"), falling back
// to runtime.GOOS; Arch is runtime.GOARCH ("arm64").
type HostInfo struct {
	Hostname string
	OS       string
	Arch     string
}

// HostMetrics is one full telemetry snapshot: the host's identity plus
// every sample collected in that pass (plans/003-host-telemetry.md).
type HostMetrics struct {
	Host    HostInfo
	Samples []MetricSample
}

// MetricsSettings is the enabled/disabled state of each collector kind,
// read from the METRICS_<COLLECTOR> config toggles.
type MetricsSettings struct {
	Temperature bool
	Fans        bool
	CPU         bool
	Memory      bool
	Disk        bool
	Uptime      bool
	Network     bool
}

// Enabled reports whether c is switched on in s. An unknown Collector value
// (not one of the seven consts above) reports false rather than defaulting
// to on, so a typo'd or future collector never runs unexpectedly.
func (s MetricsSettings) Enabled(c Collector) bool {
	switch c {
	case CollectorTemperature:
		return s.Temperature
	case CollectorFans:
		return s.Fans
	case CollectorCPU:
		return s.CPU
	case CollectorMemory:
		return s.Memory
	case CollectorDisk:
		return s.Disk
	case CollectorUptime:
		return s.Uptime
	case CollectorNetwork:
		return s.Network
	default:
		return false
	}
}
