//go:build linux

package monitor

import (
	"os"
	"strings"
)

// SMBIOSUUID reads the machine's SMBIOS UUID straight from sysfs, avoiding
// a dmidecode dependency in the installed OS (some minimal images don't
// ship dmidecode, and it needs root — the sysfs path needs neither).
// Returns "" when unavailable; callers decide whether that's fatal.
func SMBIOSUUID() string {
	b, err := os.ReadFile("/sys/class/dmi/id/product_uuid")
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}
