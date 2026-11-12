// bootmode.go probes whether the live boot environment was reached via
// UEFI or legacy BIOS. Both install paths are supported: cloud-init seed
// files are written into the target rootfs (mode-independent), GRUB uses
// the ESP path for UEFI and the BIOS Boot Partition / MBR gap for legacy.
// The operator can pin the expected mode per install (spec.BootMode) so a
// firmware mismatch fails before the target disk is touched.
package installer

import "fmt"

// DetectBootMode returns "uefi" when /sys/firmware/efi/efivars exists,
// otherwise "bios". An FS error from the probe is surfaced verbatim so
// callers can distinguish "no efivars dir" from "couldn't read sysfs".
func DetectBootMode(fs FS) (string, error) {
	if fs == nil {
		return "", fmt.Errorf("install: bootmode: fs is nil")
	}
	const efivarsPath = "/sys/firmware/efi/efivars"
	if fs.Exists(efivarsPath) {
		// Confirm it's a directory; a stray file with the same name is
		// not a legitimate UEFI boot signal.
		fi, err := fs.Stat(efivarsPath)
		if err != nil {
			return "", fmt.Errorf("install: stat %s: %w", efivarsPath, err)
		}
		if fi.IsDir() {
			return "uefi", nil
		}
	}
	return "bios", nil
}

// ValidateRequestedBootMode enforces the install-time selection from the
// controller. Empty and "auto" mean "follow the mode the machine actually
// booted in"; "uefi"/"bios" require the live boot mode to match. A mismatch
// must fail before any disk write — installing the wrong bootloader on a
// machine whose firmware boots the other way produces an unbootable system.
func ValidateRequestedBootMode(requested, detected string) error {
	switch requested {
	case "", "auto":
		return nil
	case "uefi", "bios":
	default:
		return fmt.Errorf("install: boot mode %q: must be auto, uefi or bios", requested)
	}
	if requested != detected {
		return fmt.Errorf(
			"install: boot mode mismatch: requested %s, machine booted %s. "+
				"Set the firmware boot mode (UEFI/Legacy BIOS) to %s in the BMC/BIOS setup, then retry the install",
			requested, detected, requested)
	}
	return nil
}

// RequireUEFI was removed once legacy BIOS installs became supported. Use
// ValidateRequestedBootMode (spec.BootMode) for the operator-selected mode.
