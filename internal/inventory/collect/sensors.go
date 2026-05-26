package collect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectSensors(ctx context.Context, r *inventory.Report) error {
	if _, err := os.Stat("/dev/ipmi0"); err != nil {
		return errors.New("no IPMI device")
	}
	out, err := runCmd(ctx, 8*time.Second, "ipmitool", "sdr", "elist", "full")
	if err != nil {
		return err
	}
	r.Sensors = parseSDR(out)
	return nil
}

// parseSDR parses the pipe-separated rows produced by `ipmitool sdr elist full`.
// Format example:
//
//   CPU1 Temp        | 01h | ok  |  3.1 | 35 degrees C
//   Fan1A            | 30h | ok  |  7.1 | 4800 RPM
//   PS1 Current      | 03h | ok  | 10.1 | 0.40 Amps
//
// Columns: Name | SensorID | Status | EntityID | Reading.
func parseSDR(out []byte) []inventory.Sensor {
	var sensors []inventory.Sensor
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 5 {
			continue
		}
		name := strings.TrimSpace(cols[0])
		status := strings.TrimSpace(cols[2])
		reading := strings.TrimSpace(cols[4])
		if name == "" {
			continue
		}
		val, unit := parseReading(reading)
		typ := inferSensorType(name, unit)
		s := inventory.Sensor{
			Name:   name,
			Type:   typ,
			Value:  val,
			Unit:   unit,
			Status: strings.ToLower(status),
		}
		sensors = append(sensors, s)
	}
	return sensors
}

// parseReading turns "35 degrees C" / "4800 RPM" / "0.40 Amps" / "no reading"
// into (numeric, unit). Returns (0, "") on unparseable.
func parseReading(s string) (float64, string) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "no reading") || strings.EqualFold(s, "disabled") || strings.EqualFold(s, "na") {
		return 0, ""
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, ""
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, ""
	}
	unit := strings.TrimSpace(strings.Join(fields[1:], " "))
	return v, unit
}

func inferSensorType(name, unit string) string {
	lname := strings.ToLower(name)
	lunit := strings.ToLower(unit)
	switch {
	case strings.Contains(lunit, "degrees") || strings.Contains(lname, "temp"):
		return "temperature"
	case strings.Contains(lunit, "rpm") || strings.Contains(lname, "fan"):
		return "fan"
	case strings.Contains(lunit, "volt") || strings.Contains(lname, "voltage") || strings.HasPrefix(lname, "vr ") || strings.HasSuffix(lname, " v") || strings.Contains(lname, "vcore"):
		return "voltage"
	case strings.Contains(lunit, "amp") || strings.Contains(lname, "current"):
		return "current"
	}
	return ""
}
