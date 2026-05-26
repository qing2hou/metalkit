// diskpick.go implements the profile-driven target-disk selection policy.
// Parsing /sys or lsblk output is delegated to the injected DiskLister so
// this file only contains pure logic — easy to test against synthesised
// Disk lists without poking at real /dev/.
//
// Selection modes match profiles.TargetDisk.Mode:
//
//   - smallest: smallest non-removable, non-readonly, transport != usb.
//     Tie-break on Name for determinism.
//   - by-path:  exact match on Disk.ByPath  (== sel.Value).
//   - by-wwn:   exact match on Disk.WWN.
//   - by-model: exact match on Disk.Model.
//
// Any "not found" returns an error whose message contains the string
// "disk not found" so tests and the agent reporter can pattern-match.
package installer

import (
	"fmt"
	"sort"

	"metalkit/internal/profiles"
)

// PickDisk applies the selection policy from sel to disks and returns the
// chosen Disk. The slice is not mutated.
func PickDisk(disks []Disk, sel profiles.TargetDisk) (Disk, error) {
	if len(disks) == 0 {
		return Disk{}, fmt.Errorf("install: disk not found: no candidates from lsblk")
	}
	switch sel.Mode {
	case "smallest", "":
		return pickSmallest(disks)
	case "by-path":
		return pickByField(disks, "by-path", sel.Value, func(d Disk) string { return d.ByPath })
	case "by-wwn":
		return pickByField(disks, "by-wwn", sel.Value, func(d Disk) string { return d.WWN })
	case "by-model":
		return pickByField(disks, "by-model", sel.Value, func(d Disk) string { return d.Model })
	default:
		return Disk{}, fmt.Errorf("install: target_disk.mode %q: unsupported", sel.Mode)
	}
}

// pickSmallest filters out removable, readonly, and USB-attached disks
// then returns the smallest by SizeBytes (ties broken by Name).
func pickSmallest(disks []Disk) (Disk, error) {
	candidates := make([]Disk, 0, len(disks))
	for _, d := range disks {
		if d.Removable || d.ReadOnly {
			continue
		}
		if d.Transport == "usb" {
			continue
		}
		if d.SizeBytes <= 0 {
			// lsblk sometimes reports 0 for empty card readers; skip.
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return Disk{}, fmt.Errorf("install: disk not found: no fixed non-USB disks")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].SizeBytes != candidates[j].SizeBytes {
			return candidates[i].SizeBytes < candidates[j].SizeBytes
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates[0], nil
}

// pickByField is the shared body of the three by-* modes. modeName and
// extract differ; everything else (empty value rejection, scan, error
// shape) is the same.
func pickByField(disks []Disk, modeName, value string, extract func(Disk) string) (Disk, error) {
	if value == "" {
		return Disk{}, fmt.Errorf("install: target_disk.mode=%s requires a value", modeName)
	}
	for _, d := range disks {
		if extract(d) == value {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("install: disk not found: %s=%q", modeName, value)
}
