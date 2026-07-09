package domain

import "testing"

func TestMetricsSettings_Enabled(t *testing.T) {
	t.Parallel()

	allOn := MetricsSettings{Temperature: true, Fans: true, CPU: true, Memory: true, Disk: true, Uptime: true, Network: true}
	allOff := MetricsSettings{}

	tests := []struct {
		name      string
		settings  MetricsSettings
		collector Collector
		want      bool
	}{
		{"temperature on", allOn, CollectorTemperature, true},
		{"fans on", allOn, CollectorFans, true},
		{"cpu on", allOn, CollectorCPU, true},
		{"memory on", allOn, CollectorMemory, true},
		{"disk on", allOn, CollectorDisk, true},
		{"uptime on", allOn, CollectorUptime, true},
		{"network on", allOn, CollectorNetwork, true},
		{"temperature off", allOff, CollectorTemperature, false},
		{"fans off", allOff, CollectorFans, false},
		{"cpu off", allOff, CollectorCPU, false},
		{"memory off", allOff, CollectorMemory, false},
		{"disk off", allOff, CollectorDisk, false},
		{"uptime off", allOff, CollectorUptime, false},
		{"network off", allOff, CollectorNetwork, false},
		{"one collector enabled leaves the others off", MetricsSettings{Fans: true}, CollectorCPU, false},
		{"one collector enabled reports itself on", MetricsSettings{Fans: true}, CollectorFans, true},
		{"unknown collector value is always false", allOn, Collector("bogus"), false},
		{"empty collector value is always false", allOn, Collector(""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.settings.Enabled(tt.collector); got != tt.want {
				t.Fatalf("Enabled(%q) = %v, want %v", tt.collector, got, tt.want)
			}
		})
	}
}
