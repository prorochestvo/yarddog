//go:build linux

package infrastructure

import (
	"strings"
	"testing"

	"github.com/prorochestvo/yarddog/domain"
)

func TestLinuxCollector_Collect(t *testing.T) {
	t.Parallel()

	allOn := domain.MetricsSettings{Temperature: true, Fans: true, CPU: true, Memory: true, Disk: true, Uptime: true, Network: true}

	t.Run("arm64 fixture (Raspberry Pi 5 capture): thermal_zone and hwmon sensors, no fans", func(t *testing.T) {
		t.Parallel()

		c := &linuxCollector{root: "testdata/metrics/arm64", settings: allOn, diskMount: t.TempDir()}
		m := c.Collect(t.Context())

		wantTemps := map[string]float64{"cpu-thermal": 52.35, "cpu_thermal": 52.35, "rp1_adc": 54.31}
		gotTemps := samplesByName(m.Samples, domain.CollectorTemperature)
		if len(gotTemps) != len(wantTemps) {
			t.Fatalf("temperature samples = %+v, want exactly %v", gotTemps, wantTemps)
		}
		for name, want := range wantTemps {
			s, ok := gotTemps[name]
			if !ok {
				t.Fatalf("missing temperature sample %q: %+v", name, gotTemps)
			}
			if !s.OK || s.Value != want || s.Unit != "celsius" {
				t.Fatalf("temperature %q = %+v, want ok celsius %v", name, s, want)
			}
		}

		fans := samplesFor(m.Samples, domain.CollectorFans)
		if len(fans) != 1 || fans[0].OK || fans[0].Error == "" {
			t.Fatalf("fans = %+v, want exactly one unavailable row (no fan sensors present)", fans)
		}

		cpu := samplesFor(m.Samples, domain.CollectorCPU)
		if len(cpu) != 3 {
			t.Fatalf("cpu samples = %+v, want 3 (load1/5/15)", cpu)
		}

		mem := samplesFor(m.Samples, domain.CollectorMemory)
		if len(mem) != 3 {
			t.Fatalf("memory samples = %+v, want 3 (total/free/available)", mem)
		}

		up := samplesFor(m.Samples, domain.CollectorUptime)
		if len(up) != 1 || !up[0].OK {
			t.Fatalf("uptime = %+v, want exactly one ok row", up)
		}

		net := samplesFor(m.Samples, domain.CollectorNetwork)
		if len(net) == 0 {
			t.Fatal("network samples = 0, want rx/tx rows for eth0")
		}
		for _, s := range net {
			if strings.HasPrefix(s.Name, "lo ") {
				t.Fatalf("network sample %+v: loopback must be excluded", s)
			}
		}

		if m.Host.Hostname == "" || m.Host.OS == "" || m.Host.Arch == "" {
			t.Fatalf("Host = %+v, want every field populated", m.Host)
		}
	})

	t.Run("amd64 fixture: fan sensor present, coretemp per-core, malformed siblings dropped", func(t *testing.T) {
		t.Parallel()

		c := &linuxCollector{root: "testdata/metrics/amd64", settings: allOn, diskMount: t.TempDir()}
		m := c.Collect(t.Context())

		// hwmon1 also carries a fan2_input fixture containing "garbage"
		// alongside the good fan1_input: it must be skipped
		// (parseHwmonFan fails, the loop continues) without dropping the
		// good sibling.
		fans := samplesByName(m.Samples, domain.CollectorFans)
		if len(fans) != 1 {
			t.Fatalf("fan samples = %+v, want exactly 1 (fan2_input is malformed and must be dropped)", fans)
		}
		if fan, ok := fans["nct6775 fan1"]; !ok || !fan.OK || fan.Value != 1200 || fan.Unit != "rpm" {
			t.Fatalf(`fan "nct6775 fan1" = %+v (present=%v), want an ok row at 1200 RPM`, fan, ok)
		}

		// hwmon0 also carries a temp3_input fixture containing
		// "garbage": it must be skipped (parseThermalTemp fails, the
		// loop continues) without dropping the two good siblings
		// alongside it.
		temps := samplesByName(m.Samples, domain.CollectorTemperature)
		if len(temps) != 2 {
			t.Fatalf("temperature samples = %+v, want exactly 2 (temp3_input is malformed and must be dropped)", temps)
		}
		wantTemps := map[string]float64{"coretemp temp1": 45, "coretemp temp2": 42}
		for name, want := range wantTemps {
			s, ok := temps[name]
			if !ok || !s.OK || s.Value != want {
				t.Fatalf("temperature %q = %+v (present=%v), want an ok sample at %v", name, s, ok, want)
			}
		}
	})

	t.Run("empty fixture: every collector reports unavailable, never panics", func(t *testing.T) {
		t.Parallel()

		c := &linuxCollector{root: "testdata/metrics/empty", settings: allOn, diskMount: t.TempDir()}
		m := c.Collect(t.Context())

		for _, collector := range []domain.Collector{
			domain.CollectorTemperature, domain.CollectorFans, domain.CollectorCPU,
			domain.CollectorMemory, domain.CollectorUptime, domain.CollectorNetwork,
		} {
			samples := samplesFor(m.Samples, collector)
			if len(samples) == 0 {
				t.Fatalf("%s: no samples at all, want an unavailable row", collector)
			}
			for _, s := range samples {
				if s.OK {
					t.Fatalf("%s: sample %+v is ok, want unavailable (fixture has nothing to read)", collector, s)
				}
			}
		}
	})

	t.Run("a disabled collector produces zero rows for that collector", func(t *testing.T) {
		t.Parallel()

		settings := allOn
		settings.Fans = false
		c := &linuxCollector{root: "testdata/metrics/arm64", settings: settings, diskMount: t.TempDir()}
		m := c.Collect(t.Context())

		if fans := samplesFor(m.Samples, domain.CollectorFans); len(fans) != 0 {
			t.Fatalf("fans = %+v, want zero rows (collector disabled)", fans)
		}
	})

	t.Run("disk collector reports a plausible sample against a real temp dir", func(t *testing.T) {
		t.Parallel()

		c := &linuxCollector{root: "testdata/metrics/empty", settings: allOn, diskMount: t.TempDir()}
		m := c.Collect(t.Context())

		disk := samplesFor(m.Samples, domain.CollectorDisk)
		if len(disk) != 5 {
			t.Fatalf("disk samples = %+v, want 5 (total/free/avail/used/used_ratio)", disk)
		}
		for _, s := range disk {
			if !s.OK || s.Value < 0 {
				t.Fatalf("disk sample %+v, want a non-negative ok reading against a real tmp filesystem", s)
			}
		}
	})
}

// samplesFor filters samples down to those matching collector, in scan
// order.
func samplesFor(samples []domain.MetricSample, collector domain.Collector) []domain.MetricSample {
	var out []domain.MetricSample
	for _, s := range samples {
		if s.Collector == collector {
			out = append(out, s)
		}
	}
	return out
}

// samplesByName indexes samplesFor(samples, collector) by Name, for
// asserting against specific sensor names regardless of scan order.
func samplesByName(samples []domain.MetricSample, collector domain.Collector) map[string]domain.MetricSample {
	out := map[string]domain.MetricSample{}
	for _, s := range samplesFor(samples, collector) {
		out[s.Name] = s
	}
	return out
}
