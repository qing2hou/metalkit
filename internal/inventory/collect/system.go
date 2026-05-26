package collect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectSystem(ctx context.Context, r *inventory.Report) error {
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		r.System.KernelRelease = strings.TrimSpace(string(data))
	} else if out, err := runCmd(ctx, 8*time.Second, "uname", "-r"); err == nil {
		r.System.KernelRelease = strings.TrimSpace(string(out))
	}
	if data, err := os.ReadFile("/proc/cmdline"); err == nil {
		r.System.KernelCmdline = strings.TrimSpace(string(data))
	}
	if h, err := os.Hostname(); err == nil {
		r.System.Hostname = h
	}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		up := parseUptime(data)
		r.System.UptimeSeconds = up
		r.System.BootTime = time.Now().Unix() - up
	}
	if data, err := os.ReadFile("/etc/metalkit-live-version"); err == nil {
		r.System.LiveImageVersion = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile("/proc/mounts"); err == nil {
		r.System.Mounts = parseMounts(data)
	} else {
		return fmt.Errorf("read /proc/mounts: %w", err)
	}
	return nil
}

func parseUptime(data []byte) int64 {
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

var pseudoFS = map[string]bool{
	"proc":       true,
	"sysfs":      true,
	"cgroup":     true,
	"cgroup2":    true,
	"devpts":     true,
	"devtmpfs":   true,
	"tmpfs":      true,
	"securityfs": true,
	"debugfs":    true,
	"tracefs":    true,
	"pstore":     true,
	"bpf":        true,
	"fusectl":    true,
	"configfs":   true,
	"efivarfs":   true,
	"none":       true,
	"autofs":     true,
	"mqueue":     true,
	"hugetlbfs":  true,
	"binfmt_misc": true,
	"rpc_pipefs": true,
	"nsfs":       true,
}

func parseMounts(data []byte) []inventory.Mount {
	var mounts []inventory.Mount
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if pseudoFS[fields[2]] {
			continue
		}
		mounts = append(mounts, inventory.Mount{
			Source: fields[0],
			Target: fields[1],
			FSType: fields[2],
			Opts:   fields[3],
		})
	}
	return mounts
}
