// bootmode.go probes whether the live boot environment was reached via
// UEFI or legacy BIOS. M2.3 only supports UEFI installs (the seed-ISO-on-
// ESP + grub-install --target=x86_64-efi path is UEFI-specific), so
// RequireUEFI is the policy gate Run uses to fail fast on misconfigured
// hosts rather than producing an unbootable disk.
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

// RequireUEFI returns nil when the boot mode is UEFI, and a descriptive
// error otherwise. M2.3 callers use this as a policy gate before doing
// anything destructive.
func RequireUEFI(fs FS) error {
	mode, err := DetectBootMode(fs)
	if err != nil {
		return err
	}
	if mode != "uefi" {
		return fmt.Errorf("install: boot mode %q: metalkit M2.3 requires UEFI", mode)
	}
	return nil
}
