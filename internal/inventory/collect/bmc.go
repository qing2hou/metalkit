package collect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectBMC(ctx context.Context, r *inventory.Report) error {
	if _, err := os.Stat("/dev/ipmi0"); err != nil {
		return errors.New("no IPMI device")
	}
	bmc := &inventory.BMC{}
	if out, err := runCmd(ctx, 8*time.Second, "ipmitool", "mc", "info"); err == nil {
		applyMcInfo(bmc, out)
	}
	if out, err := runCmd(ctx, 8*time.Second, "ipmitool", "lan", "print", "1"); err == nil {
		applyLanPrint(bmc, out)
	} else if out2, err2 := runCmd(ctx, 8*time.Second, "ipmitool", "lan", "print", "8"); err2 == nil {
		applyLanPrint(bmc, out2)
	}
	if out, err := runCmd(ctx, 8*time.Second, "ipmitool", "fru", "list"); err == nil {
		bmc.FRU = parseFRU(out)
	}
	r.BMC = bmc
	return nil
}

func applyMcInfo(b *inventory.BMC, out []byte) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, ok := splitKV(sc.Text())
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Manufacturer Name":
			b.Vendor = val
		case "Product ID":
			b.ProductID = val
		case "Firmware Revision":
			b.FirmwareVersion = val
		case "IPMI Version":
			b.IPMIVersion = val
		}
	}
}

func applyLanPrint(b *inventory.BMC, out []byte) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, ok := splitKV(sc.Text())
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "MAC Address":
			b.MAC = val
		case "IP Address":
			b.IP = val
		case "Default Gateway IP":
			b.Gateway = val
		case "Subnet Mask":
			b.Subnet = val
		case "802.1q VLAN ID":
			// "Disabled" or numeric.
			if v := atoiSafe(val); v > 0 {
				b.VLAN = v
			}
		}
	}
}

// parseFRU extracts the first "Builtin FRU Device" or "(ID 0)" record.
func parseFRU(out []byte) *inventory.FRU {
	fru := &inventory.FRU{}
	any := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	inBuiltin := false
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if trim == "" {
			// Blank line ends a FRU block. If we already populated the builtin
			// block, stop reading further.
			if inBuiltin && any {
				return fru
			}
			inBuiltin = false
			continue
		}
		if strings.HasPrefix(trim, "FRU Device Description") {
			inBuiltin = strings.Contains(trim, "Builtin FRU Device") || strings.Contains(trim, "(ID 0)")
			continue
		}
		if !inBuiltin {
			continue
		}
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Board Mfg":
			fru.BoardMfg = val
			any = true
		case "Board Product":
			fru.BoardProduct = val
			any = true
		case "Board Serial":
			fru.BoardSerial = val
			any = true
		case "Product Serial":
			fru.ProductSerial = val
			any = true
		}
	}
	if !any {
		return nil
	}
	return fru
}
