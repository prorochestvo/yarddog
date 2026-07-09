package infrastructure

import (
	"runtime"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestParseThermalTemp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    float64
		wantErr bool
	}{
		{"pi5 thermal_zone0 cpu-thermal", "52350", 52.35, false},
		{"pi5 hwmon2 rp1_adc, trailing newline", "54310\n", 54.31, false},
		{"below-zero ambient", "-500", -0.5, false},
		{"malformed value errors, never panics", "not-a-number", 0, true},
		{"empty string errors", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseThermalTemp(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseThermalTemp(%q) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseThermalTemp(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parseThermalTemp(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseHwmonFan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    float64
		wantErr bool
	}{
		{"amd64 fixture fan RPM", "1200\n", 1200, false},
		{"zero RPM (stalled or stopped fan)", "0", 0, false},
		{"malformed value errors, never panics", "spinning", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseHwmonFan(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseHwmonFan(%q) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHwmonFan(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parseHwmonFan(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseLoadavg(t *testing.T) {
	t.Parallel()

	t.Run("pi5 sample", func(t *testing.T) {
		t.Parallel()

		l1, l5, l15, err := parseLoadavg("0.52 0.58 0.59 1/234 5678")
		if err != nil {
			t.Fatalf("parseLoadavg() error = %v", err)
		}
		if l1 != 0.52 || l5 != 0.58 || l15 != 0.59 {
			t.Fatalf("parseLoadavg() = (%v, %v, %v), want (0.52, 0.58, 0.59)", l1, l5, l15)
		}
	})

	t.Run("trailing newline is tolerated", func(t *testing.T) {
		t.Parallel()

		l1, l5, l15, err := parseLoadavg("1.00 2.00 3.00 1/234 5678\n")
		if err != nil {
			t.Fatalf("parseLoadavg() error = %v", err)
		}
		if l1 != 1 || l5 != 2 || l15 != 3 {
			t.Fatalf("parseLoadavg() = (%v, %v, %v), want (1, 2, 3)", l1, l5, l15)
		}
	})

	t.Run("too few fields errors, never panics", func(t *testing.T) {
		t.Parallel()

		if _, _, _, err := parseLoadavg("0.52 0.58"); err == nil {
			t.Fatal("parseLoadavg() error = nil, want an error for too few fields")
		}
	})

	t.Run("malformed field errors", func(t *testing.T) {
		t.Parallel()

		if _, _, _, err := parseLoadavg("busy 0.58 0.59 1/234 5678"); err == nil {
			t.Fatal("parseLoadavg() error = nil, want an error for a non-numeric field")
		}
	})
}

func TestParseMeminfo(t *testing.T) {
	t.Parallel()

	t.Run("pi5 sample: total/free/available parsed to bytes, other lines ignored", func(t *testing.T) {
		t.Parallel()

		raw := "MemTotal:        8131764 kB\n" +
			"MemFree:         3245112 kB\n" +
			"MemAvailable:    6103224 kB\n" +
			"Buffers:          123456 kB\n" +
			"Cached:          2734567 kB\n" +
			"SwapTotal:             0 kB\n" +
			"SwapFree:              0 kB\n"

		total, free, available, err := parseMeminfo(raw)
		if err != nil {
			t.Fatalf("parseMeminfo() error = %v", err)
		}
		if total != 8131764*1024 {
			t.Fatalf("total = %d, want %d", total, uint64(8131764*1024))
		}
		if free != 3245112*1024 {
			t.Fatalf("free = %d, want %d", free, uint64(3245112*1024))
		}
		if available != 6103224*1024 {
			t.Fatalf("available = %d, want %d", available, uint64(6103224*1024))
		}
	})

	t.Run("missing MemAvailable errors (older kernels lack it)", func(t *testing.T) {
		t.Parallel()

		raw := "MemTotal:  8131764 kB\nMemFree:  3245112 kB\n"
		if _, _, _, err := parseMeminfo(raw); err == nil {
			t.Fatal("parseMeminfo() error = nil, want an error when MemAvailable is absent")
		}
	})

	t.Run("missing MemTotal errors", func(t *testing.T) {
		t.Parallel()

		raw := "MemFree:  3245112 kB\nMemAvailable: 6103224 kB\n"
		if _, _, _, err := parseMeminfo(raw); err == nil {
			t.Fatal("parseMeminfo() error = nil, want an error when MemTotal is absent")
		}
	})

	t.Run("malformed value errors, never panics", func(t *testing.T) {
		t.Parallel()

		raw := "MemTotal: not-a-number kB\nMemFree: 1 kB\nMemAvailable: 1 kB\n"
		if _, _, _, err := parseMeminfo(raw); err == nil {
			t.Fatal("parseMeminfo() error = nil, want an error for a non-numeric MemTotal")
		}
	})
}

func TestParseUptime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    float64
		wantErr bool
	}{
		{"typical /proc/uptime", "12345.67 2345.60", 12345.67, false},
		{"pi5-shaped uptime", "355407.85 1517258.99\n", 355407.85, false},
		{"empty input errors", "", 0, true},
		{"malformed first field errors", "not-a-number 123", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseUptime(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseUptime(%q) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUptime(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parseUptime(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseNetDev(t *testing.T) {
	t.Parallel()

	const raw = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  123456     543    0    0    0     0          0         0    123456     543    0    0    0     0       0          0
  eth0: 987654321  654321    0    0    0     0          0        12  123456789  234567    0    0    0     0       0          0
`

	t.Run("excludes loopback and returns rx/tx bytes for real interfaces", func(t *testing.T) {
		t.Parallel()

		got := parseNetDev(raw)
		if len(got) != 1 {
			t.Fatalf("parseNetDev() = %+v, want exactly 1 interface (lo excluded)", got)
		}
		if got[0].Name != "eth0" || got[0].RxBytes != 987654321 || got[0].TxBytes != 123456789 {
			t.Fatalf("parseNetDev()[0] = %+v, want eth0 rx=987654321 tx=123456789", got[0])
		}
	})

	t.Run("only loopback present returns an empty slice", func(t *testing.T) {
		t.Parallel()

		onlyLo := "Inter-|   Receive\n face |bytes\n    lo:  1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n"
		got := parseNetDev(onlyLo)
		if len(got) != 0 {
			t.Fatalf("parseNetDev() = %+v, want empty (only loopback present)", got)
		}
	})

	t.Run("a line with no colon is skipped, other interfaces still parsed", func(t *testing.T) {
		t.Parallel()

		malformed := "Inter-|   Receive\n face |bytes\nnot-a-valid-line\n  eth0: 987654321  654321    0    0    0     0          0        12  123456789  234567    0    0    0     0       0          0\n"
		got := parseNetDev(malformed)
		if len(got) != 1 || got[0].Name != "eth0" || got[0].RxBytes != 987654321 || got[0].TxBytes != 123456789 {
			t.Fatalf("parseNetDev() = %+v, want exactly eth0 (the colon-less line dropped, not the whole file)", got)
		}
	})

	t.Run("too few fields on one interface line is skipped, others still parsed", func(t *testing.T) {
		t.Parallel()

		malformed := "Inter-|   Receive\n face |bytes\n  eth0: 1 2 3\n  eth1: 987654321  654321    0    0    0     0          0        12  123456789  234567    0    0    0     0       0          0\n"
		got := parseNetDev(malformed)
		if len(got) != 1 || got[0].Name != "eth1" || got[0].RxBytes != 987654321 || got[0].TxBytes != 123456789 {
			t.Fatalf("parseNetDev() = %+v, want exactly eth1 (eth0's too-few-fields line dropped, not the whole file)", got)
		}
	})
}

func TestParseSysctlLoadavg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		l1, l5, l15 float64
		wantErr     bool
	}{
		{"typical macOS sysctl output", "{ 0.52 0.58 0.59 }", 0.52, 0.58, 0.59, false},
		{"trailing newline tolerated", "{ 1.00 2.00 3.00 }\n", 1.00, 2.00, 3.00, false},
		{"too few fields errors", "{ 0.52 0.58 }", 0, 0, 0, true},
		{"malformed field errors", "{ busy 0.58 0.59 }", 0, 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l1, l5, l15, err := parseSysctlLoadavg(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSysctlLoadavg(%q) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSysctlLoadavg(%q) error = %v, want nil", tt.raw, err)
			}
			if l1 != tt.l1 || l5 != tt.l5 || l15 != tt.l15 {
				t.Fatalf("parseSysctlLoadavg(%q) = (%v, %v, %v), want (%v, %v, %v)", tt.raw, l1, l5, l15, tt.l1, tt.l5, tt.l15)
			}
		})
	}
}

func TestParseSysctlUint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    uint64
		wantErr bool
	}{
		{"hw.memsize sample (16 GiB)", "17179869184\n", 17179869184, false},
		{"no trailing newline", "8589934592", 8589934592, false},
		{"malformed value errors", "sixteen gigs", 0, true},
		{"negative value errors", "-1", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSysctlUint(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSysctlUint(%q) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSysctlUint(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parseSysctlUint(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseSysctlBoottime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{"typical macOS kern.boottime output", "{ sec = 1712345678, usec = 123456 } Fri Apr  5 12:14:38 2024\n", 1712345678, false},
		{"zero usec", "{ sec = 1700000000, usec = 0 } Tue Nov 14 22:13:20 2023\n", 1700000000, false},
		{"usec listed before sec still finds the real sec field", "{ usec = 999999, sec = 1712345678 } Fri Apr  5 12:14:38 2024\n", 1712345678, false},
		{"only usec present errors instead of misreading it as sec", "{ usec = 123456 }", 0, true},
		{"no \"sec\" anywhere errors", "unexpected sysctl output", 0, true},
		{"malformed sec value errors", "{ sec = not-a-number, usec = 0 }", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSysctlBoottime(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSysctlBoottime(%q) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSysctlBoottime(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parseSysctlBoottime(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseVmStat(t *testing.T) {
	t.Parallel()

	t.Run("typical macOS vm_stat output", func(t *testing.T) {
		t.Parallel()

		raw := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
			"Pages free:                              210701.\n" +
			"Pages active:                            145339.\n" +
			"Pages inactive:                          138263.\n" +
			"Pages speculative:                         5411.\n" +
			"Pages throttled:                               0.\n" +
			"Pages wired down:                          88886.\n" +
			"Pages purgeable:                           12951.\n" +
			"\"Translation faults\":                 553113127.\n" +
			"Pages copy-on-write:                     9962929.\n"

		pageSize, pages, err := parseVmStat(raw)
		if err != nil {
			t.Fatalf("parseVmStat() error = %v", err)
		}
		if pageSize != 16384 {
			t.Fatalf("pageSize = %d, want 16384", pageSize)
		}
		if pages["Pages free"] != 210701 {
			t.Fatalf(`pages["Pages free"] = %d, want 210701`, pages["Pages free"])
		}
		if pages["Pages inactive"] != 138263 {
			t.Fatalf(`pages["Pages inactive"] = %d, want 138263`, pages["Pages inactive"])
		}
		if pages["Pages speculative"] != 5411 {
			t.Fatalf(`pages["Pages speculative"] = %d, want 5411`, pages["Pages speculative"])
		}
	})

	t.Run("missing page size header errors", func(t *testing.T) {
		t.Parallel()

		if _, _, err := parseVmStat("garbage header\nPages free: 1.\n"); err == nil {
			t.Fatal("parseVmStat() error = nil, want an error when the page-size header is missing")
		}
	})

	t.Run("malformed page count errors, never panics", func(t *testing.T) {
		t.Parallel()

		raw := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: not-a-number.\n"
		if _, _, err := parseVmStat(raw); err == nil {
			t.Fatal("parseVmStat() error = nil, want an error for a malformed page count")
		}
	})
}

func TestParseOSRelease(t *testing.T) {
	t.Parallel()

	t.Run("quoted PRETTY_NAME", func(t *testing.T) {
		t.Parallel()

		content := "NAME=\"Ubuntu\"\nVERSION_ID=\"26.04\"\nPRETTY_NAME=\"Ubuntu 26.04 LTS\"\nID=ubuntu\n"
		if got := parseOSRelease(content); got != "Ubuntu 26.04 LTS" {
			t.Fatalf("parseOSRelease() = %q, want %q", got, "Ubuntu 26.04 LTS")
		}
	})

	t.Run("unquoted PRETTY_NAME", func(t *testing.T) {
		t.Parallel()

		if got := parseOSRelease("PRETTY_NAME=Debian\n"); got != "Debian" {
			t.Fatalf("parseOSRelease() = %q, want %q", got, "Debian")
		}
	})

	t.Run("single-quoted value", func(t *testing.T) {
		t.Parallel()

		if got := parseOSRelease("PRETTY_NAME='Alpine Linux v3.20'"); got != "Alpine Linux v3.20" {
			t.Fatalf("parseOSRelease() = %q, want %q", got, "Alpine Linux v3.20")
		}
	})

	t.Run("no PRETTY_NAME yields empty for GOOS fallback", func(t *testing.T) {
		t.Parallel()

		if got := parseOSRelease("NAME=\"Ubuntu\"\nVERSION_ID=\"26.04\"\n"); got != "" {
			t.Fatalf("parseOSRelease() = %q, want empty", got)
		}
	})

	t.Run("empty content", func(t *testing.T) {
		t.Parallel()

		if got := parseOSRelease(""); got != "" {
			t.Fatalf("parseOSRelease() = %q, want empty", got)
		}
	})
}

func TestParseSwVers(t *testing.T) {
	t.Parallel()

	t.Run("name and version", func(t *testing.T) {
		t.Parallel()

		content := "ProductName:\tmacOS\nProductVersion:\t15.2\nBuildVersion:\t24C101\n"
		if got := parseSwVers(content); got != "macOS 15.2" {
			t.Fatalf("parseSwVers() = %q, want %q", got, "macOS 15.2")
		}
	})

	t.Run("name only", func(t *testing.T) {
		t.Parallel()

		if got := parseSwVers("ProductName:\tmacOS\n"); got != "macOS" {
			t.Fatalf("parseSwVers() = %q, want %q", got, "macOS")
		}
	})

	t.Run("version without name yields empty for GOOS fallback", func(t *testing.T) {
		t.Parallel()

		if got := parseSwVers("ProductVersion:\t15.2\n"); got != "" {
			t.Fatalf("parseSwVers() = %q, want empty", got)
		}
	})

	t.Run("empty content", func(t *testing.T) {
		t.Parallel()

		if got := parseSwVers(""); got != "" {
			t.Fatalf("parseSwVers() = %q, want empty", got)
		}
	})
}

func TestDiskUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		total, free uint64
		wantUsed    uint64
		wantRatio   float64
	}{
		{"worked example", 100, 40, 60, 0.6},
		{"empty filesystem", 100, 100, 0, 0},
		{"full filesystem", 100, 0, 100, 1},
		{"zero total is not a division by zero", 0, 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			used, ratio := diskUsage(tt.total, tt.free)
			if used != tt.wantUsed || ratio != tt.wantRatio {
				t.Fatalf("diskUsage(%d, %d) = (%d, %v), want (%d, %v)", tt.total, tt.free, used, ratio, tt.wantUsed, tt.wantRatio)
			}
		})
	}
}

func TestDiskSamples(t *testing.T) {
	t.Parallel()

	assertSamples := func(t *testing.T, got, want []domain.MetricSample) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("diskSamples() = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("diskSamples()[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	}

	t.Run("the root mount drops the \"/ \" name prefix", func(t *testing.T) {
		t.Parallel()

		assertSamples(t, diskSamples("/", 100, 40, 30, 60, 0.6), []domain.MetricSample{
			{Collector: domain.CollectorDisk, Name: "total", Value: 100, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "free", Value: 40, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "avail", Value: 30, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "used", Value: 60, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "used_ratio", Value: 0.6, Unit: "ratio", OK: true},
		})
	})

	t.Run("a non-root mount keeps its prefix", func(t *testing.T) {
		t.Parallel()

		assertSamples(t, diskSamples("/data", 100, 40, 30, 60, 0.6), []domain.MetricSample{
			{Collector: domain.CollectorDisk, Name: "/data total", Value: 100, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "/data free", Value: 40, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "/data avail", Value: 30, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "/data used", Value: 60, Unit: "bytes", OK: true},
			{Collector: domain.CollectorDisk, Name: "/data used_ratio", Value: 0.6, Unit: "ratio", OK: true},
		})
	})
}

func TestUptimeSeconds(t *testing.T) {
	t.Parallel()

	boottime := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	now := boottime.Add(90 * time.Minute)

	got := uptimeSeconds(boottime.Unix(), now)
	want := (90 * time.Minute).Seconds()
	if got != want {
		t.Fatalf("uptimeSeconds() = %v, want %v", got, want)
	}
}

func TestDarwinAvailableBytes(t *testing.T) {
	t.Parallel()

	pages := map[string]uint64{
		"Pages free":        100,
		"Pages inactive":    50,
		"Pages speculative": 10,
		"Pages active":      9999, // must not be counted
	}

	got := darwinAvailableBytes(16384, pages)
	want := uint64(160) * 16384
	if got != want {
		t.Fatalf("darwinAvailableBytes() = %d, want %d", got, want)
	}
}

func TestHostInfo(t *testing.T) {
	t.Parallel()

	h := hostInfo()

	if h.Hostname == "" {
		t.Fatal("Hostname is empty, want a real hostname or the \"unknown\" fallback")
	}
	// OS is osDescription()'s result: a distro/product string when detectable,
	// else the runtime.GOOS fallback — either way non-empty. The exact value
	// depends on the runner, so assert non-empty here and pin the parsing in
	// TestParseOSRelease/TestParseSwVers.
	if h.OS == "" {
		t.Fatal("OS is empty, want a description or the runtime.GOOS fallback")
	}
	if h.Arch != runtime.GOARCH {
		t.Fatalf("Arch = %q, want runtime.GOARCH %q", h.Arch, runtime.GOARCH)
	}
}

func TestUnavailable(t *testing.T) {
	t.Parallel()

	s := unavailable(domain.CollectorFans, "fans", "no fan sensors present")

	if s.Collector != domain.CollectorFans {
		t.Fatalf("Collector = %q, want %q", s.Collector, domain.CollectorFans)
	}
	if s.Name != "fans" {
		t.Fatalf("Name = %q, want %q", s.Name, "fans")
	}
	if s.OK {
		t.Fatal("OK = true, want false")
	}
	if s.Error != "no fan sensors present" {
		t.Fatalf("Error = %q, want %q", s.Error, "no fan sensors present")
	}
	if s.Value != 0 {
		t.Fatalf("Value = %v, want the zero value", s.Value)
	}
}
