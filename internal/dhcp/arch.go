package dhcp

import "github.com/insomniacslk/dhcp/dhcpv4"

type ClientArch uint16

const (
	ArchBIOSx86    ClientArch = 0x0000
	ArchUEFIIA32   ClientArch = 0x0006
	ArchUEFIx64    ClientArch = 0x0007
	ArchUEFIx64Alt ClientArch = 0x0009
	ArchUEFIArm64  ClientArch = 0x000b
)

func (a ClientArch) String() string {
	switch a {
	case ArchBIOSx86:
		return "bios-x86"
	case ArchUEFIIA32:
		return "uefi-ia32"
	case ArchUEFIx64:
		return "uefi-x64"
	case ArchUEFIx64Alt:
		return "uefi-x64-alt"
	case ArchUEFIArm64:
		return "uefi-arm64"
	}
	return "unknown"
}

func bootfileForArch(a ClientArch) (string, bool) {
	switch a {
	case ArchBIOSx86:
		return "undionly.kpxe", true
	case ArchUEFIIA32:
		return "ipxe.efi", true
	case ArchUEFIx64, ArchUEFIx64Alt:
		return "snponly.efi", true
	case ArchUEFIArm64:
		return "arm64-snponly.efi", true
	}
	return "", false
}

func selectBootfile(req *dhcpv4.DHCPv4) (string, bool) {
	if req == nil {
		return "", false
	}
	archs := req.ClientArch()
	if len(archs) == 0 {
		return "", false
	}
	return bootfileForArch(ClientArch(archs[0]))
}
