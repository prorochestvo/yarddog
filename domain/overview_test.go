package domain

import "testing"

func TestLossPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sent, received int
		want           int
	}{
		{"no loss", 5, 5, 0},
		{"total loss", 5, 0, 100},
		{"a fractional loss rounds half away from zero", 8, 5, 38}, // (1-5/8)*100 = 37.5 -> 38
		{"sent<=0 reports 0 rather than dividing by zero", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := LossPercent(tt.sent, tt.received); got != tt.want {
				t.Fatalf("LossPercent(%d, %d) = %d, want %d", tt.sent, tt.received, got, tt.want)
			}
		})
	}
}
