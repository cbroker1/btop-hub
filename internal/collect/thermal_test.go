package collect

import "testing"

func TestCPUTjMax(t *testing.T) {
	for _, c := range []struct {
		model string
		want  float64
	}{
		{"Ryzen 9 7950X", 95},      // the GPU rig
		{"Ryzen 9 9950X3D", 95},    // the gaming rig
		{"Ryzen AI Max+ 395", 100}, // the Z13
		{"Ryzen AI Max 390", 100},
		{"Ryzen AI Max+ PRO 395", 100},
		{"Ryzen 7 7800X3D", 89},
		{"Ryzen 9 7950X3D", 89},
		{"Ryzen 7 9800X3D", 95},
		{"Ryzen 5 7600", 95},
		{"Ryzen 9 5950X", 0},     // not in the table: never flagged
		{"Ryzen AI 9 HX 370", 0}, // a laptop chip, not Strix Halo
		{"Core i9-13900K", 0},    // Intel's comes from coretemp, not the table
		{"", 0},
	} {
		got, _, ok := cpuTjMax(c.model)
		if got != c.want || ok != (c.want > 0) {
			t.Errorf("cpuTjMax(%q) = %v, %v; want %v", c.model, got, ok, c.want)
		}
	}
}

func TestAtLimit(t *testing.T) {
	for _, c := range []struct {
		temp, limit float64
		want        bool
	}{
		{85, 95, false}, // the A6000's 85 °C would not trip a 95 °C part
		{93.9, 95, false},
		{94, 95, true}, // held at the limit, reading just under it
		{96, 95, true},
		{120, 0, false}, // unknown limit: never
	} {
		if got := atLimit(c.temp, c.limit); got != c.want {
			t.Errorf("atLimit(%v, %v) = %v, want %v", c.temp, c.limit, got, c.want)
		}
	}
}
