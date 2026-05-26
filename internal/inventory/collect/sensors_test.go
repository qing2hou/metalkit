package collect

import (
	"testing"
)

func TestParseSDR_HappyPath(t *testing.T) {
	sensors := parseSDR(readFixture(t, "ipmitool-sdr.txt"))
	if len(sensors) < 6 {
		t.Fatalf("expected >=6 sensors, got %d", len(sensors))
	}
	got := map[string]struct {
		Type  string
		Value float64
		Unit  string
	}{}
	for _, s := range sensors {
		got[s.Name] = struct {
			Type  string
			Value float64
			Unit  string
		}{s.Type, s.Value, s.Unit}
	}

	cpu1 := got["CPU1 Temp"]
	if cpu1.Type != "temperature" {
		t.Errorf("CPU1 Temp type = %q", cpu1.Type)
	}
	if cpu1.Value != 35 {
		t.Errorf("CPU1 Temp value = %f", cpu1.Value)
	}
	if cpu1.Unit != "degrees C" {
		t.Errorf("CPU1 Temp unit = %q", cpu1.Unit)
	}

	fan := got["Fan1A"]
	if fan.Type != "fan" {
		t.Errorf("Fan1A type = %q", fan.Type)
	}
	if fan.Value != 4800 {
		t.Errorf("Fan1A value = %f", fan.Value)
	}

	volt := got["PS1 Voltage"]
	if volt.Type != "voltage" {
		t.Errorf("PS1 Voltage type = %q", volt.Type)
	}

	cur := got["PS1 Current"]
	if cur.Type != "current" {
		t.Errorf("PS1 Current type = %q", cur.Type)
	}
	if cur.Value != 0.40 {
		t.Errorf("PS1 Current value = %f", cur.Value)
	}
}

func TestParseSDR_Empty(t *testing.T) {
	s := parseSDR(nil)
	if len(s) != 0 {
		t.Errorf("expected 0 sensors, got %d", len(s))
	}
}

func TestParseReading(t *testing.T) {
	cases := []struct {
		in     string
		val    float64
		unit   string
	}{
		{"35 degrees C", 35, "degrees C"},
		{"4800 RPM", 4800, "RPM"},
		{"0.40 Amps", 0.40, "Amps"},
		{"No Reading", 0, ""},
		{"", 0, ""},
	}
	for _, c := range cases {
		v, u := parseReading(c.in)
		if v != c.val || u != c.unit {
			t.Errorf("parseReading(%q) = (%f, %q), want (%f, %q)", c.in, v, u, c.val, c.unit)
		}
	}
}
