package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectDisks(ctx context.Context, r *inventory.Report) error {
	out, err := runCmd(ctx, 8*time.Second, "lsblk", "-d", "-b", "-J", "-O")
	if err != nil {
		return fmt.Errorf("lsblk: %w", err)
	}
	disks, err := parseLsblk(out)
	if err != nil {
		return fmt.Errorf("parse lsblk: %w", err)
	}
	for i := range disks {
		d := &disks[i]
		if d.Removable {
			continue
		}
		if sout, err := runCmd(ctx, 8*time.Second, "smartctl", "-j", "-a", d.Path); err == nil {
			if s := parseSmartctl(sout); s != nil {
				d.SMART = s
			}
		}
		if d.Transport == "nvme" {
			n := &inventory.NVMe{}
			populated := false
			if iout, err := runCmd(ctx, 8*time.Second, "nvme", "id-ctrl", "-o", "json", d.Path); err == nil {
				if ns := parseNvmeIdCtrl(iout); ns > 0 {
					n.Namespaces = ns
					populated = true
				}
			}
			if lout, err := runCmd(ctx, 8*time.Second, "nvme", "smart-log", "-o", "json", d.Path); err == nil {
				if mt := parseNvmeSmartLog(lout); mt > 0 {
					n.MaxTempC = mt
					populated = true
				}
			}
			if populated {
				d.NVMe = n
			}
		}
	}
	r.Disks = disks
	return nil
}

// lsblkOut mirrors `lsblk -J -O` shape. We keep only fields we read.
type lsblkOut struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string  `json:"name"`
	KName      string  `json:"kname"`
	Type       string  `json:"type"`
	Size       *uint64 `json:"size"`
	Model      string  `json:"model"`
	Serial     string  `json:"serial"`
	Rev        string  `json:"rev"`
	WWN        string  `json:"wwn"`
	Tran       string  `json:"tran"`
	Vendor     string  `json:"vendor"`
	Rota       *bool   `json:"rota"`
	RM         *bool   `json:"rm"`
	HCTL       string  `json:"hctl"`
	PKName     string  `json:"pkname"`
	Subsystems string  `json:"subsystems"`
}

func parseLsblk(out []byte) ([]inventory.Disk, error) {
	var l lsblkOut
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, err
	}
	var disks []inventory.Disk
	for _, d := range l.BlockDevices {
		if d.Type != "disk" {
			continue
		}
		name := d.Name
		if name == "" {
			name = d.KName
		}
		path := "/dev/" + name
		var size uint64
		if d.Size != nil {
			size = *d.Size
		}
		disk := inventory.Disk{
			KName:     d.KName,
			Path:      path,
			Type:      d.Type,
			SizeBytes: size,
			Model:     strings.TrimSpace(d.Model),
			Serial:    strings.TrimSpace(d.Serial),
			Firmware:  strings.TrimSpace(d.Rev),
			Vendor:    strings.TrimSpace(d.Vendor),
			WWN:       strings.TrimSpace(d.WWN),
			Transport: strings.ToLower(strings.TrimSpace(d.Tran)),
		}
		if d.Rota != nil {
			disk.Rotational = *d.Rota
		}
		if d.RM != nil {
			disk.Removable = *d.RM
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

// smartctl -j output (we cherry-pick).
type smartctlOut struct {
	SmartStatus struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	PowerOnTime struct {
		Hours int64 `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount int64 `json:"power_cycle_count"`
	Temperature     struct {
		Current int `json:"current"`
	} `json:"temperature"`
	ATASmartAttributes struct {
		Table []struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			Raw   struct {
				Value uint64 `json:"value"`
				Str   string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMeSmartHealthInformationLog struct {
		DataUnitsRead    uint64 `json:"data_units_read"`
		DataUnitsWritten uint64 `json:"data_units_written"`
		MediaErrors      int    `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
}

func parseSmartctl(out []byte) *inventory.SMART {
	var s smartctlOut
	if err := json.Unmarshal(out, &s); err != nil {
		return nil
	}
	res := &inventory.SMART{}
	if s.SmartStatus.Passed {
		res.Health = "PASSED"
	}
	res.PowerOnHours = s.PowerOnTime.Hours
	res.PowerCycles = s.PowerCycleCount
	res.TemperatureC = s.Temperature.Current
	for _, a := range s.ATASmartAttributes.Table {
		switch a.ID {
		case 5: // Reallocated_Sector_Ct
			res.ReallocatedSectors = int64(a.Raw.Value)
		case 197: // Current_Pending_Sector
			res.PendingSectors = int64(a.Raw.Value)
		case 241: // Total_LBAs_Written (in LBAs; we leave units to the SAT layer)
			res.BytesWritten = a.Raw.Value * 512
		case 242: // Total_LBAs_Read
			res.BytesRead = a.Raw.Value * 512
		}
	}
	// NVMe values override if present (data_units = 512000 bytes).
	if s.NVMeSmartHealthInformationLog.DataUnitsRead > 0 {
		res.BytesRead = s.NVMeSmartHealthInformationLog.DataUnitsRead * 512_000
	}
	if s.NVMeSmartHealthInformationLog.DataUnitsWritten > 0 {
		res.BytesWritten = s.NVMeSmartHealthInformationLog.DataUnitsWritten * 512_000
	}
	if s.NVMeSmartHealthInformationLog.MediaErrors > 0 {
		res.SMARTErrors = s.NVMeSmartHealthInformationLog.MediaErrors
	}
	return res
}

type nvmeIDCtrl struct {
	Nn int `json:"nn"`
}

func parseNvmeIdCtrl(out []byte) int {
	var n nvmeIDCtrl
	if err := json.Unmarshal(out, &n); err != nil {
		return 0
	}
	return n.Nn
}

type nvmeSmartLog struct {
	Temperature int `json:"temperature"`
}

// parseNvmeSmartLog returns the controller temperature converted to celsius.
// nvme-cli reports temperature in Kelvin.
func parseNvmeSmartLog(out []byte) int {
	var s nvmeSmartLog
	if err := json.Unmarshal(out, &s); err != nil {
		return 0
	}
	if s.Temperature <= 0 {
		return 0
	}
	return s.Temperature - 273
}
