package inventory

import "time"

// SchemaVersion bumps when the on-the-wire JSON layout changes in a breaking way.
// Reports persisted with an older version are still queryable; consumers branch
// on this field.
const SchemaVersion = 1

// Report is the full inventory snapshot sent by the agent. The schema is
// deliberately wide — collect every fact a single tool can extract about
// the box. Optional sub-objects use pointers so they serialize as null when
// the underlying source was unavailable (e.g. no BMC on a VM).
type Report struct {
	SchemaVersion        int       `json:"schema_version"`
	AgentVersion         string    `json:"agent_version"`
	CollectedAt          time.Time `json:"collected_at"`
	CollectionDurationMS int64     `json:"collection_duration_ms"`

	Machine      Machine        `json:"machine"`
	Firmware     Firmware       `json:"firmware"`
	CPU          CPU            `json:"cpu"`
	Memory       Memory         `json:"memory"`
	Disks        []Disk         `json:"disks"`
	NICs         []NIC          `json:"nics"`
	PCIDevices   []PCIDevice    `json:"pci_devices"`
	Accelerators []Accelerator  `json:"accelerators"`
	BMC          *BMC           `json:"bmc,omitempty"`
	Sensors      []Sensor       `json:"sensors"`
	System       System         `json:"system"`
	Agent        AgentMeta      `json:"agent"`
}

// Machine — top-level identity from SMBIOS.
type Machine struct {
	SMBIOSUUID   string    `json:"smbios_uuid"`
	Manufacturer string    `json:"manufacturer,omitempty"`
	ProductName  string    `json:"product_name,omitempty"`
	Version      string    `json:"version,omitempty"`
	Serial       string    `json:"serial,omitempty"`
	SKU          string    `json:"sku,omitempty"`
	Family       string    `json:"family,omitempty"`
	Baseboard    Baseboard `json:"baseboard"`
	Chassis      Chassis   `json:"chassis"`
}

type Baseboard struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Version      string `json:"version,omitempty"`
	Serial       string `json:"serial,omitempty"`
	AssetTag     string `json:"asset_tag,omitempty"`
}

type Chassis struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Type         string `json:"type,omitempty"`
	Serial       string `json:"serial,omitempty"`
	AssetTag     string `json:"asset_tag,omitempty"`
}

// Firmware — BIOS, UEFI mode, Secure Boot, TPM.
type Firmware struct {
	BIOS        BIOS    `json:"bios"`
	UEFIMode    bool    `json:"uefi_mode"`
	SecureBoot  *bool   `json:"secure_boot,omitempty"`
	TPM         *TPM    `json:"tpm,omitempty"`
}

type BIOS struct {
	Vendor      string `json:"vendor,omitempty"`
	Version     string `json:"version,omitempty"`
	ReleaseDate string `json:"release_date,omitempty"`
	Revision    string `json:"revision,omitempty"`
}

type TPM struct {
	Present      bool   `json:"present"`
	Version      string `json:"version,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
}

// CPU — topology and per-socket details.
type CPU struct {
	Sockets      int        `json:"sockets"`
	TotalCores   int        `json:"total_cores"`
	TotalThreads int        `json:"total_threads"`
	NUMANodes    int        `json:"numa_nodes"`
	Vendor       string     `json:"vendor,omitempty"`
	Arch         string     `json:"arch,omitempty"`
	PerSocket    []CPUEntry `json:"per_socket"`
	Flags        []string   `json:"flags,omitempty"`
}

type CPUEntry struct {
	Socket       string `json:"socket"`
	Model        string `json:"model,omitempty"`
	Cores        int    `json:"cores"`
	Threads      int    `json:"threads"`
	BaseFreqMHz  int    `json:"base_freq_mhz,omitempty"`
	MaxFreqMHz   int    `json:"max_freq_mhz,omitempty"`
	Microcode    string `json:"microcode,omitempty"`
	L1KB         int    `json:"l1_kb,omitempty"`
	L2KB         int    `json:"l2_kb,omitempty"`
	L3KB         int    `json:"l3_kb,omitempty"`
}

// Memory — total + per-DIMM.
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes,omitempty"`
	ECC            bool   `json:"ecc"`
	DIMMs          []DIMM `json:"dimms"`
}

type DIMM struct {
	Locator             string `json:"locator"`
	Bank                string `json:"bank,omitempty"`
	SizeBytes           uint64 `json:"size_bytes"`
	Type                string `json:"type,omitempty"`
	SpeedMTS            int    `json:"speed_mts,omitempty"`
	ConfiguredSpeedMTS  int    `json:"configured_speed_mts,omitempty"`
	Manufacturer        string `json:"manufacturer,omitempty"`
	Serial              string `json:"serial,omitempty"`
	PartNumber          string `json:"part_number,omitempty"`
	Rank                int    `json:"rank,omitempty"`
	VoltageV            string `json:"voltage,omitempty"`
}

// Disk — one entry per block device (NVMe / SATA / SAS / virtio).
type Disk struct {
	KName       string  `json:"kname"`
	Path        string  `json:"path"`
	Type        string  `json:"type"` // disk, part, lvm, md
	SizeBytes   uint64  `json:"size_bytes"`
	Model       string  `json:"model,omitempty"`
	Serial      string  `json:"serial,omitempty"`
	Firmware    string  `json:"firmware,omitempty"`
	Rotational  bool    `json:"rotational"`
	Transport   string  `json:"transport,omitempty"` // nvme, sata, sas
	WWN         string  `json:"wwn,omitempty"`
	Vendor      string  `json:"vendor,omitempty"`
	Removable   bool    `json:"removable"`
	PCIAddress  string  `json:"pci_address,omitempty"`
	SMART       *SMART  `json:"smart,omitempty"`
	NVMe        *NVMe   `json:"nvme,omitempty"`
}

type SMART struct {
	Health             string `json:"health,omitempty"` // PASSED / FAILED
	PowerOnHours       int64  `json:"power_on_hours,omitempty"`
	PowerCycles        int64  `json:"power_cycles,omitempty"`
	TemperatureC       int    `json:"temperature_c,omitempty"`
	BytesRead          uint64 `json:"bytes_read,omitempty"`
	BytesWritten       uint64 `json:"bytes_written,omitempty"`
	ReallocatedSectors int64  `json:"reallocated_sectors,omitempty"`
	PendingSectors     int64  `json:"pending_sectors,omitempty"`
	SMARTErrors        int    `json:"smart_errors,omitempty"`
}

type NVMe struct {
	Namespaces int `json:"namespaces,omitempty"`
	MaxTempC   int `json:"max_temp_c,omitempty"`
}

// NIC — one per network interface, including the down ones.
type NIC struct {
	Name            string   `json:"name"`
	MAC             string   `json:"mac"`
	PermanentMAC    string   `json:"permanent_mac,omitempty"`
	SpeedMbps       int      `json:"speed_mbps,omitempty"`
	Duplex          string   `json:"duplex,omitempty"`
	Link            bool     `json:"link"`
	MTU             int      `json:"mtu,omitempty"`
	Driver          string   `json:"driver,omitempty"`
	DriverVersion   string   `json:"driver_version,omitempty"`
	FirmwareVersion string   `json:"firmware_version,omitempty"`
	PCIAddress      string   `json:"pci_address,omitempty"`
	BusInfo         string   `json:"bus_info,omitempty"`
	Addresses       []string `json:"addresses,omitempty"` // CIDR
	SFP             *SFP     `json:"sfp,omitempty"`
}

type SFP struct {
	Vendor       string `json:"vendor,omitempty"`
	PartNumber   string `json:"part_number,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Type         string `json:"type,omitempty"`
	WavelengthNM int    `json:"wavelength_nm,omitempty"`
}

// PCIDevice — every PCI device on the bus.
type PCIDevice struct {
	Address         string   `json:"address"`
	ClassID         string   `json:"class_id,omitempty"`
	ClassName       string   `json:"class_name,omitempty"`
	VendorID        string   `json:"vendor_id,omitempty"`
	VendorName      string   `json:"vendor_name,omitempty"`
	DeviceID        string   `json:"device_id,omitempty"`
	DeviceName      string   `json:"device_name,omitempty"`
	SubsystemVendor string   `json:"subsystem_vendor,omitempty"`
	SubsystemDevice string   `json:"subsystem_device,omitempty"`
	Driver          string   `json:"driver,omitempty"`
	KernelModules   []string `json:"kernel_modules,omitempty"`
	LinkSpeed       string   `json:"link_speed,omitempty"`
	LinkWidth       string   `json:"link_width,omitempty"`
}

// Accelerator — GPU / TPU / FPGA, derived from PCI class.
type Accelerator struct {
	PCIAddress string `json:"pci_address"`
	Vendor     string `json:"vendor,omitempty"`
	Model      string `json:"model,omitempty"`
	Class      string `json:"class,omitempty"` // gpu, 3d, processing
	VRAMBytes  uint64 `json:"vram_bytes,omitempty"`
	Driver     string `json:"driver,omitempty"`
}

// BMC — out-of-band management controller.
type BMC struct {
	Vendor          string `json:"vendor,omitempty"`
	ProductID       string `json:"product_id,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
	IPMIVersion     string `json:"ipmi_version,omitempty"`
	MAC             string `json:"mac,omitempty"`
	IP              string `json:"ip,omitempty"`
	Gateway         string `json:"gateway,omitempty"`
	Subnet          string `json:"subnet,omitempty"`
	VLAN            int    `json:"vlan_id,omitempty"`
	FRU             *FRU   `json:"fru,omitempty"`
}

type FRU struct {
	BoardMfg      string `json:"board_mfg,omitempty"`
	BoardProduct  string `json:"board_product,omitempty"`
	BoardSerial   string `json:"board_serial,omitempty"`
	ProductSerial string `json:"product_serial,omitempty"`
}

// Sensor — IPMI SDR reading. Boot-once snapshot (M3 will stream).
type Sensor struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"` // temperature, fan, voltage, current
	Value        float64 `json:"value"`
	Unit         string  `json:"unit,omitempty"`
	Status       string  `json:"status,omitempty"`
	ThresholdMin float64 `json:"threshold_min,omitempty"`
	ThresholdMax float64 `json:"threshold_max,omitempty"`
}

// System — kernel + OS state.
type System struct {
	KernelRelease    string   `json:"kernel_release"`
	KernelCmdline    string   `json:"kernel_cmdline,omitempty"`
	Hostname         string   `json:"hostname,omitempty"`
	BootTime         int64    `json:"boot_time,omitempty"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	LiveImageVersion string   `json:"live_image_version,omitempty"`
	Mounts           []Mount  `json:"mounts,omitempty"`
}

type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
	FSType string `json:"fstype"`
	Opts   string `json:"opts,omitempty"`
}

// AgentMeta — what the agent did and any partial failures it accumulated.
type AgentMeta struct {
	Version              string         `json:"version"`
	CollectedAt          time.Time      `json:"collected_at"`
	CollectionDurationMS int64          `json:"collection_duration_ms"`
	Errors               []CollectError `json:"errors,omitempty"`
}

// CollectError — one collector's failure does not stop the others.
type CollectError struct {
	Collector string `json:"collector"`
	Err       string `json:"err"`
}
