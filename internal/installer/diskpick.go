// diskpick.go implements the profile-driven target-disk selection policy.
// Parsing /sys or lsblk output is delegated to the injected DiskLister so
// this file only contains pure logic — easy to test against synthesised
// Disk lists without poking at real /dev/.
//
// Selection modes match profiles.TargetDisk.Mode:
//
//   - smallest: smallest non-removable, non-readonly, transport != usb.
//     Tie-break on Name for determinism.
//   - by-path:  match on Disk.ByPath (the /dev/disk/by-path/… symlink) with
//     DevPath (/dev/sda) and Name (sda) fallbacks — see pickByPath.
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
		if sel.Value == "" {
			return Disk{}, fmt.Errorf("install: target_disk.mode=%s requires a value", "by-path")
		}
		return pickByPath(disks, sel.Value)
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

// pickByPath matches the selector value against each disk's stable by-path
// symlink (Disk.ByPath). The value is normalised so the forms the UI and
// operators naturally write all resolve to the same disk:
//
//   - /dev/disk/by-path/pci-…  → exact match on ByPath
//   - /dev/sda                 → match on DevPath
//   - sda                      → match on Name (kernel name)
//
// The DevPath/Name fallbacks are deliberate: they identify the same disk on
// a machine whose boot order doesn't shuffle device names (no removable
// media), and failing with "not found" when the operator picked a perfectly
// visible /dev/sda from the machine's own report would be a foot-gun. Pick
// the first disk that matches; on multi-path setups a later alias would
// point at the same underlying device anyway.
func pickByPath(disks []Disk, value string) (Disk, error) {
	for _, d := range disks {
		if d.ByPath == value || d.DevPath == value || d.Name == value {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("install: disk not found: by-path=%q", value)
}

// pickByField is the shared body of the two by-* modes that compare a single
// exact field. modeName and extract differ; everything else (empty value
// rejection, scan, error shape) is the same.
func pickByField(disks []Disk, modeName, value string, extract func(Disk) string) (Disk, error) {
	if value == "" {
		return Disk{}, fmt.Errorf("install: target_disk.mode=%s requires a value", modeName)
	}
	return scanByField(disks, modeName, value, extract)
}

func scanByField(disks []Disk, modeName, value string, extract func(Disk) string) (Disk, error) {
	for _, d := range disks {
		if extract(d) == value {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("install: disk not found: %s=%q", modeName, value)
}
