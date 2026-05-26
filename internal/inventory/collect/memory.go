package collect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"metalkit/internal/inventory"
)

func collectMemory(_ context.Context, r *inventory.Report) error {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return fmt.Errorf("read /proc/meminfo: %w", err)
	}
	total, avail := parseMeminfo(data)
	r.Memory.TotalBytes = total
	r.Memory.AvailableBytes = avail
	return nil
}

func parseMeminfo(data []byte) (uint64, uint64) {
	var total, avail uint64
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		fields := strings.Fields(val)
		if len(fields) < 1 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo values are in kB (kibibytes).
		bytes := n * 1024
		switch key {
		case "MemTotal":
			total = bytes
		case "MemAvailable":
			avail = bytes
		}
	}
	return total, avail
}
