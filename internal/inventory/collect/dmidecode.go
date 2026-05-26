package collect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectDMIDecode(ctx context.Context, r *inventory.Report) error {
	sysOut, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "system")
	if err != nil {
		return fmt.Errorf("dmidecode -t system: %w", err)
	}
	m, err := parseSystemInfo(sysOut)
	if err != nil {
		return fmt.Errorf("parse system: %w", err)
	}
	r.Machine.SMBIOSUUID = m.SMBIOSUUID
	r.Machine.Manufacturer = m.Manufacturer
	r.Machine.ProductName = m.ProductName
	r.Machine.Version = m.Version
	r.Machine.Serial = m.Serial
	r.Machine.SKU = m.SKU
	r.Machine.Family = m.Family

	if bbOut, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "2"); err == nil {
		r.Machine.Baseboard = parseBaseboard(bbOut)
	}
	if chOut, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "3"); err == nil {
		r.Machine.Chassis = parseChassis(chOut)
	}
	if biosOut, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "0"); err == nil {
		r.Firmware.BIOS = parseBIOS(biosOut)
	}
	if memArr, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "16"); err == nil {
		r.Memory.ECC = parseMemoryArrayECC(memArr)
	}
	if dimmOut, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "17"); err == nil {
		r.Memory.DIMMs = parseDIMMs(dimmOut)
	}
	return nil
}

// dmiBlock is a parsed "Handle 0xNNNN, DMI type N, M bytes" record. Lines like
// "\tKey: value" become map[Key]=value. Multi-line list values (Characteristics)
// are ignored — we don't need them.
type dmiBlock struct {
	dmiType int
	kv      map[string]string
}

func parseDMIBlocks(data []byte) []dmiBlock {
	var blocks []dmiBlock
	var cur *dmiBlock
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Handle ") {
			// Start of a new block. Parse "DMI type N".
			if cur != nil {
				blocks = append(blocks, *cur)
			}
			cur = &dmiBlock{kv: make(map[string]string)}
			if idx := strings.Index(line, "DMI type "); idx >= 0 {
				rest := line[idx+len("DMI type "):]
				if comma := strings.Index(rest, ","); comma > 0 {
					if n, err := strconv.Atoi(strings.TrimSpace(rest[:comma])); err == nil {
						cur.dmiType = n
					}
				}
			}
			continue
		}
		if cur == nil {
			continue
		}
		// Top-level section title line (no leading tab) is informational only.
		if !strings.HasPrefix(line, "\t") {
			continue
		}
		// We only care about depth-1 ("\tKey: value") lines. Deeper indented
		// list entries (e.g. "\t\tBIOS supports …") are skipped.
		if strings.HasPrefix(line, "\t\t") {
			continue
		}
		kv := strings.TrimPrefix(line, "\t")
		colon := strings.Index(kv, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(kv[:colon])
		val := strings.TrimSpace(kv[colon+1:])
		cur.kv[key] = val
	}
	if cur != nil {
		blocks = append(blocks, *cur)
	}
	return blocks
}

func parseSystemInfo(out []byte) (inventory.Machine, error) {
	var m inventory.Machine
	for _, b := range parseDMIBlocks(out) {
		if b.dmiType != 1 {
			continue
		}
		m.Manufacturer = dmiClean(b.kv["Manufacturer"])
		m.ProductName = dmiClean(b.kv["Product Name"])
		m.Version = dmiClean(b.kv["Version"])
		m.Serial = dmiClean(b.kv["Serial Number"])
		m.SMBIOSUUID = strings.ToLower(dmiClean(b.kv["UUID"]))
		m.SKU = dmiClean(b.kv["SKU Number"])
		m.Family = dmiClean(b.kv["Family"])
		return m, nil
	}
	return m, nil
}

func parseBaseboard(out []byte) inventory.Baseboard {
	var bb inventory.Baseboard
	for _, b := range parseDMIBlocks(out) {
		if b.dmiType != 2 {
			continue
		}
		bb.Manufacturer = dmiClean(b.kv["Manufacturer"])
		bb.Product = dmiClean(b.kv["Product Name"])
		bb.Version = dmiClean(b.kv["Version"])
		bb.Serial = dmiClean(b.kv["Serial Number"])
		bb.AssetTag = dmiClean(b.kv["Asset Tag"])
		return bb
	}
	return bb
}

func parseChassis(out []byte) inventory.Chassis {
	var c inventory.Chassis
	for _, b := range parseDMIBlocks(out) {
		if b.dmiType != 3 {
			continue
		}
		c.Manufacturer = dmiClean(b.kv["Manufacturer"])
		c.Type = dmiClean(b.kv["Type"])
		c.Serial = dmiClean(b.kv["Serial Number"])
		c.AssetTag = dmiClean(b.kv["Asset Tag"])
		return c
	}
	return c
}

func parseBIOS(out []byte) inventory.BIOS {
	var b inventory.BIOS
	for _, blk := range parseDMIBlocks(out) {
		if blk.dmiType != 0 {
			continue
		}
		b.Vendor = dmiClean(blk.kv["Vendor"])
		b.Version = dmiClean(blk.kv["Version"])
		b.ReleaseDate = dmiClean(blk.kv["Release Date"])
		b.Revision = dmiClean(blk.kv["BIOS Revision"])
		return b
	}
	return b
}

func parseMemoryArrayECC(out []byte) bool {
	for _, blk := range parseDMIBlocks(out) {
		if blk.dmiType != 16 {
			continue
		}
		v := strings.ToLower(blk.kv["Error Correction Type"])
		if v == "" || v == "none" {
			return false
		}
		return strings.Contains(v, "ecc")
	}
	return false
}

func parseDIMMs(out []byte) []inventory.DIMM {
	var dimms []inventory.DIMM
	for _, blk := range parseDMIBlocks(out) {
		if blk.dmiType != 17 {
			continue
		}
		size := blk.kv["Size"]
		if size == "" || strings.Contains(strings.ToLower(size), "no module") || strings.Contains(strings.ToLower(size), "not installed") {
			continue
		}
		d := inventory.DIMM{
			Locator:            dmiClean(blk.kv["Locator"]),
			Bank:               dmiClean(blk.kv["Bank Locator"]),
			SizeBytes:          parseDIMMSize(size),
			Type:               dmiClean(blk.kv["Type"]),
			SpeedMTS:           parseMTPerSecond(blk.kv["Speed"]),
			ConfiguredSpeedMTS: parseMTPerSecond(blk.kv["Configured Memory Speed"]),
			Manufacturer:       dmiClean(blk.kv["Manufacturer"]),
			Serial:             dmiClean(blk.kv["Serial Number"]),
			PartNumber:         dmiClean(blk.kv["Part Number"]),
			Rank:               atoiSafe(blk.kv["Rank"]),
			VoltageV:           dmiClean(blk.kv["Configured Voltage"]),
		}
		dimms = append(dimms, d)
	}
	return dimms
}

// parseDIMMSize converts "32 GB" / "16384 MB" / "8 GiB" → bytes.
func parseDIMMSize(s string) uint64 {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(fields[1]) {
	case "kb", "kib":
		return n * 1024
	case "mb", "mib":
		return n * 1024 * 1024
	case "gb", "gib":
		return n * 1024 * 1024 * 1024
	case "tb", "tib":
		return n * 1024 * 1024 * 1024 * 1024
	}
	return 0
}

// parseMTPerSecond converts "3200 MT/s" → 3200.
func parseMTPerSecond(s string) int {
	fields := strings.Fields(s)
	if len(fields) < 1 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// dmiClean strips dmidecode's "Not Specified", "Not Provided", "Unknown", etc.
func dmiClean(s string) string {
	t := strings.TrimSpace(s)
	switch strings.ToLower(t) {
	case "not specified", "not provided", "unknown", "to be filled by o.e.m.", "default string", "none", "no asset tag":
		return ""
	}
	return t
}
