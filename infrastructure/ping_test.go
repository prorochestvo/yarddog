package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestParsePingSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          string
		wantSent     int
		wantReceived int
		wantAvgMS    float64
		wantErr      bool
	}{
		{
			name: "linux success",
			raw: "PING 1.1.1.1 (1.1.1.1) 56(84) bytes of data.\n" +
				"64 bytes from 1.1.1.1: icmp_seq=1 ttl=58 time=18.3 ms\n" +
				"64 bytes from 1.1.1.1: icmp_seq=2 ttl=58 time=23.0 ms\n" +
				"\n" +
				"--- 1.1.1.1 ping statistics ---\n" +
				"2 packets transmitted, 2 received, 0% packet loss, time 1001ms\n" +
				"rtt min/avg/max/mdev = 18.320/20.645/22.971/2.325 ms\n",
			wantSent: 2, wantReceived: 2, wantAvgMS: 20.645,
		},
		{
			name: "macOS success",
			raw: "PING 1.1.1.1 (1.1.1.1): 56 data bytes\n" +
				"64 bytes from 1.1.1.1: icmp_seq=0 ttl=59 time=11.234 ms\n" +
				"64 bytes from 1.1.1.1: icmp_seq=1 ttl=59 time=10.876 ms\n" +
				"\n" +
				"--- 1.1.1.1 ping statistics ---\n" +
				"2 packets transmitted, 2 packets received, 0.0% packet loss\n" +
				"round-trip min/avg/max/stddev = 10.876/11.055/11.234/0.179 ms\n",
			wantSent: 2, wantReceived: 2, wantAvgMS: 11.055,
		},
		{
			name: "linux 100% loss: received=0, no error",
			raw: "PING 192.0.2.1 (192.0.2.1) 56(84) bytes of data.\n" +
				"\n" +
				"--- 192.0.2.1 ping statistics ---\n" +
				"1 packets transmitted, 0 received, 100% packet loss, time 0ms\n",
			wantSent: 1, wantReceived: 0, wantAvgMS: 0,
		},
		{
			name: "macOS 100% loss: received=0, no error",
			raw: "PING 192.0.2.1 (192.0.2.1): 56 data bytes\n" +
				"\n" +
				"--- 192.0.2.1 ping statistics ---\n" +
				"2 packets transmitted, 0 packets received, 100.0% packet loss\n",
			wantSent: 2, wantReceived: 0, wantAvgMS: 0,
		},
		{
			name: "partial loss: sent=5 received=3, avg present",
			raw: "PING 1.1.1.1 (1.1.1.1) 56(84) bytes of data.\n" +
				"\n" +
				"--- 1.1.1.1 ping statistics ---\n" +
				"5 packets transmitted, 3 received, 40% packet loss, time 4003ms\n" +
				"rtt min/avg/max/mdev = 10.000/15.000/20.000/4.082 ms\n",
			wantSent: 5, wantReceived: 3, wantAvgMS: 15.000,
		},
		{
			name:    "linux DNS failure body has no summary: error",
			raw:     "ping: nonexistent.invalid.example.zzz: Name or service not known\n",
			wantErr: true,
		},
		{
			name:    "macOS DNS failure body has no summary: error",
			raw:     "ping: cannot resolve nonexistent.invalid.example.zzz: Unknown host\n",
			wantErr: true,
		},
		{
			name:    "empty output: error",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "garbage output: error",
			raw:     "this is not ping output at all\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sent, received, avgMS, err := parsePingSummary(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parsePingSummary() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePingSummary() error = %v, want nil", err)
			}
			if sent != tt.wantSent {
				t.Errorf("sent = %d, want %d", sent, tt.wantSent)
			}
			if received != tt.wantReceived {
				t.Errorf("received = %d, want %d", received, tt.wantReceived)
			}
			if avgMS != tt.wantAvgMS {
				t.Errorf("avgMS = %v, want %v", avgMS, tt.wantAvgMS)
			}
		})
	}
}

func TestPingCollector_Collect(t *testing.T) {
	t.Parallel()

	const linuxSuccess = "PING 1.1.1.1 (1.1.1.1) 56(84) bytes of data.\n" +
		"\n" +
		"--- 1.1.1.1 ping statistics ---\n" +
		"2 packets transmitted, 2 received, 0% packet loss, time 1001ms\n" +
		"rtt min/avg/max/mdev = 18.320/20.645/22.971/2.325 ms\n"
	const linuxAllLoss = "PING 192.0.2.1 (192.0.2.1) 56(84) bytes of data.\n" +
		"\n" +
		"--- 192.0.2.1 ping statistics ---\n" +
		"2 packets transmitted, 0 received, 100% packet loss, time 1001ms\n"

	errExec := errors.New("exec: \"ping\": executable file not found in $PATH")

	c := &pingCollector{
		hosts:   []string{"1.1.1.1", "unreachable.example", "192.0.2.1"},
		count:   2,
		timeout: time.Second,
		ping: func(_ context.Context, host string, _ int, _ time.Duration) (string, error) {
			switch host {
			case "1.1.1.1":
				return linuxSuccess, nil
			case "unreachable.example":
				// a truly failed exec (e.g. missing binary): no output at all.
				return "", errExec
			case "192.0.2.1":
				// 100% packet loss: ping itself exits non-zero, but its stdout is
				// still a parseable summary (see runPing's doc in ping_linux.go).
				return linuxAllLoss, errors.New("exit status 1")
			default:
				// never call t.Fatalf from this goroutine (it is not the test
				// goroutine); surface the bug as an error the assertions catch.
				return "", fmt.Errorf("test bug: unexpected host %q", host)
			}
		},
	}

	got := c.Collect(t.Context())

	if len(got) != 3 {
		t.Fatalf("Collect() = %d results, want 3", len(got))
	}

	if got[0].Host != "1.1.1.1" || !got[0].OK || got[0].Sent != 2 || got[0].Received != 2 || got[0].AvgMS != 20.645 {
		t.Fatalf("Collect()[0] = %+v, want the reachable result", got[0])
	}
	if got[0].Error != "" {
		t.Fatalf("Collect()[0].Error = %q, want empty", got[0].Error)
	}

	if got[1].Host != "unreachable.example" || got[1].OK || got[1].Error != errExec.Error() {
		t.Fatalf("Collect()[1] = %+v, want the failed-exec result with the exec error", got[1])
	}
	if got[1].Sent != c.count {
		t.Fatalf("Collect()[1].Sent = %d, want %d (the configured count, since nothing was parsed)", got[1].Sent, c.count)
	}

	if got[2].Host != "192.0.2.1" || got[2].OK || got[2].Received != 0 || got[2].Error != "" {
		t.Fatalf("Collect()[2] = %+v, want a parsed 100%%-loss result with no error", got[2])
	}
	if got[2].Sent != 2 {
		t.Fatalf("Collect()[2].Sent = %d, want 2 (parsed from the summary despite the non-zero exit)", got[2].Sent)
	}
}
