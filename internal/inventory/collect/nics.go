package collect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectNICs(ctx context.Context, r *inventory.Report) error {
	out, err := runCmd(ctx, 8*time.Second, "ip", "-j", "addr", "show")
	if err != nil {
		return fmt.Errorf("ip -j addr: %w", err)
	}
	nics, err := parseIPAddr(out)
	if err != nil {
		return fmt.Errorf("parse ip addr: %w", err)
	}
	for i := range nics {
		n := &nics[i]
		if eout, err := runCmd(ctx, 8*time.Second, "ethtool", n.Name); err == nil {
			applyEthtool(n, eout)
		}
		if iout, err := runCmd(ctx, 8*time.Second, "ethtool", "-i", n.Name); err == nil {
			applyEthtoolDrvinfo(n, iout)
		}
		if data, err := os.ReadFile("/sys/class/net/" + n.Name + "/address"); err == nil {
			perm := strings.TrimSpace(string(data))
			if perm != "" && !strings.EqualFold(perm, n.MAC) {
				n.PermanentMAC = perm
			}
		}
		if mout, err := runCmd(ctx, 8*time.Second, "ethtool", "-m", n.Name); err == nil {
			if sfp := parseEthtoolModule(mout); sfp != nil {
				n.SFP = sfp
			}
		}
	}
	r.NICs = nics
	return nil
}

type ipAddrEntry struct {
	IfName    string   `json:"ifname"`
	Address   string   `json:"address"`
	MTU       int      `json:"mtu"`
	Operstate string   `json:"operstate"`
	LinkType  string   `json:"link_type"`
	Flags     []string `json:"flags"`
	AddrInfo  []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func parseIPAddr(out []byte) ([]inventory.NIC, error) {
	var entries []ipAddrEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, err
	}
	var nics []inventory.NIC
	for _, e := range entries {
		if e.IfName == "lo" || e.LinkType == "loopback" {
			continue
		}
		n := inventory.NIC{
			Name: e.IfName,
			MAC:  e.Address,
			MTU:  e.MTU,
			Link: strings.EqualFold(e.Operstate, "UP"),
		}
		for _, a := range e.AddrInfo {
			if a.Local == "" {
				continue
			}
			n.Addresses = append(n.Addresses, fmt.Sprintf("%s/%d", a.Local, a.Prefixlen))
		}
		nics = append(nics, n)
	}
	return nics, nil
}

func applyEthtool(n *inventory.NIC, out []byte) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		switch key {
		case "Speed":
			// "1000Mb/s" or "Unknown!"
			if strings.HasSuffix(val, "Mb/s") {
				s := strings.TrimSuffix(val, "Mb/s")
				if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					n.SpeedMbps = v
				}
			}
		case "Duplex":
			if !strings.Contains(strings.ToLower(val), "unknown") {
				n.Duplex = strings.ToLower(val)
			}
		}
	}
}

func applyEthtoolDrvinfo(n *inventory.NIC, out []byte) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, ok := splitKV(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "driver":
			n.Driver = val
		case "version":
			n.DriverVersion = val
		case "firmware-version":
			n.FirmwareVersion = val
		case "bus-info":
			n.BusInfo = val
			n.PCIAddress = val
		}
	}
}

// parseEthtoolModule extracts SFP fields from `ethtool -m` text output.
func parseEthtoolModule(out []byte) *inventory.SFP {
	sfp := &inventory.SFP{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	any := false
	for sc.Scan() {
		key, val, ok := splitKV(sc.Text())
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Vendor name":
			sfp.Vendor = val
			any = true
		case "Vendor PN":
			sfp.PartNumber = val
			any = true
		case "Vendor SN":
			sfp.Serial = val
			any = true
		case "Identifier", "Transceiver type":
			if sfp.Type == "" {
				sfp.Type = val
				any = true
			}
		case "Laser wavelength":
			// "1310nm" or "1310 nm"
			s := strings.TrimSuffix(strings.TrimSpace(val), "nm")
			s = strings.TrimSpace(s)
			if v, err := strconv.Atoi(s); err == nil {
				sfp.WavelengthNM = v
				any = true
			}
		}
	}
	if !any {
		return nil
	}
	return sfp
}
