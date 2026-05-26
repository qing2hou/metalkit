package collect

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectPCI(ctx context.Context, r *inventory.Report) error {
	// -vmmnnk: verbose machine-readable, numeric+text IDs, kernel driver+modules.
	// Available since pciutils 3.1.x; Debian Bookworm ships 3.9.0, so this is safe.
	// We do NOT use -J (JSON) because it was only added in pciutils 3.10.0 (Dec 2023).
	out, err := runCmd(ctx, 8*time.Second, "lspci", "-vmmnnk")
	if err != nil {
		return fmt.Errorf("lspci: %w", err)
	}
	devices, err := parseLspci(out)
	if err != nil {
		return fmt.Errorf("parse lspci: %w", err)
	}
	for i := range devices {
		applySysfsLink(&devices[i])
	}
	r.PCIDevices = devices
	r.Accelerators = deriveAccelerators(devices)
	return nil
}

// trailing " [hex]" on Class / Vendor / Device / SVendor / SDevice values.
var idSuffix = regexp.MustCompile(`\s*\[([0-9a-fA-F]+)\]\s*$`)

func splitNameID(s string) (name, id string) {
	m := idSuffix.FindStringSubmatchIndex(s)
	if m == nil {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(s[:m[0]]), strings.ToLower(s[m[2]:m[3]])
}

// parseLspci consumes the output of `lspci -vmmnnk`: blocks of "Key:\tValue"
// lines separated by blank lines. Module can appear multiple times in one block.
func parseLspci(out []byte) ([]inventory.PCIDevice, error) {
	var devs []inventory.PCIDevice
	var cur map[string]string
	var modules []string

	flush := func() {
		if cur == nil {
			return
		}
		defer func() { cur = nil; modules = nil }()
		slot := cur["Slot"]
		if slot == "" {
			return
		}
		className, classID := splitNameID(cur["Class"])
		vendorName, vendorID := splitNameID(cur["Vendor"])
		deviceName, deviceID := splitNameID(cur["Device"])
		_, svendorID := splitNameID(cur["SVendor"])
		_, sdeviceID := splitNameID(cur["SDevice"])
		devs = append(devs, inventory.PCIDevice{
			Address:         slot,
			ClassID:         classID,
			ClassName:       className,
			VendorID:        vendorID,
			VendorName:      vendorName,
			DeviceID:        deviceID,
			DeviceName:      deviceName,
			SubsystemVendor: svendorID,
			SubsystemDevice: sdeviceID,
			Driver:          cur["Driver"],
			KernelModules:   modules,
		})
	}

	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if cur == nil {
			cur = map[string]string{}
		}
		if key == "Module" {
			if val != "" {
				modules = append(modules, val)
			}
			continue
		}
		cur[key] = val
	}
	flush()
	return devs, nil
}

func applySysfsLink(d *inventory.PCIDevice) {
	if d.Address == "" {
		return
	}
	addr := d.Address
	if !strings.HasPrefix(addr, "0000:") && strings.Count(addr, ":") == 1 {
		addr = "0000:" + addr
	}
	base := "/sys/bus/pci/devices/" + addr
	if b, err := os.ReadFile(base + "/current_link_speed"); err == nil {
		d.LinkSpeed = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(base + "/current_link_width"); err == nil {
		d.LinkWidth = strings.TrimSpace(string(b))
	}
}

func deriveAccelerators(devs []inventory.PCIDevice) []inventory.Accelerator {
	var accs []inventory.Accelerator
	for _, d := range devs {
		class := strings.ToLower(d.ClassID)
		var kind string
		switch {
		case class == "0300":
			kind = "gpu"
		case class == "0302":
			kind = "3d"
		case strings.HasPrefix(class, "12"):
			kind = "processing"
		default:
			continue
		}
		accs = append(accs, inventory.Accelerator{
			PCIAddress: d.Address,
			Vendor:     d.VendorName,
			Model:      d.DeviceName,
			Class:      kind,
			Driver:     d.Driver,
		})
	}
	return accs
}
