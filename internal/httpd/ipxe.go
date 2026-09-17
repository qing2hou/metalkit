package httpd

import (
	"bytes"
	"text/template"
)

// ipxeTemplate is the M1 chain-loaded iPXE script. The fetch= URL MUST use
// an IPv4 literal because live-boot's initramfs busybox-wget has no DNS.
//
// Only console=ttyS0 is passed — NOT console=tty0. The kernel printk burst
// during early boot floods the mgag200drmfb fbcon write path, and on iDRAC's
// virtualised VGA scanout that path can wedge indefinitely (writes to
// /dev/tty1 block forever in vfs_write). Routing kernel printk to ttyS0
// (captured by iDRAC SOL) avoids this; getty@tty1 still writes its login
// banner to /dev/tty1 → fbcon → VGA, which iDRAC vKVM renders.
//
// metalkit.url= is consumed by the in-live inventory agent (cmd/agent). It
// is the controller base URL the agent POSTs reports/heartbeats to.
//
// systemd.mask=lvm2-pvscan@.service: the live image ships lvm2 (some images
// need it) and udev otherwise auto-activates every VG it finds on the host's
// disks. On a machine whose previous OS left an LVM PV behind, that holds the
// target disk's partitions open, BLKRRPART then fails with EBUSY, the kernel
// keeps serving the OLD partition table, and the install dies at "no rootfs
// candidate partition" (Dell R630 incident 2026-09-16; the agent also
// releases holders explicitly — see installer/release.go — this mask just
// removes the most common source). Metalkit does not support LVM root images
// (rootfs detection skips LVM2_member), so nothing in the install path needs
// VG auto-activation.
// ethdevice-timeout=90: live-boot's 9990-networking.sh runs ONE klibc
// ipconfig per interface with -t $ETHDEV_TIMEOUT (default 15s, no outer
// retry) and then panics with "Unable to find a live file system on the
// network". At kernel handoff the NIC is reset (PHY renegotiates) and the
// switch port re-runs STP, so the first ~30-45s of egress frames from the
// host are blackholed — a 15s DHCP window can die entirely inside that
// blackout (Dell R630 reinstall incident 2026-09-17: boot deterministically
// dropped to (initramfs) with zero DISCOVERs reaching the DHCP server, while
// a manual `ipconfig eno1` minutes later succeeded instantly). 90s lets
// ipconfig keep retransmitting DISCOVERs through classic-STP convergence;
// the cost is a slower failure (90s) only when no DHCP server answers at all.
//
// The /boot/* URLs are path-style (/boot/<arch>/<file>) rather than
// query-style (?arch=) — live-boot's initramfs 9990-mount-http.sh derives the
// fetch archive type from a sed on the extension suffix and the query string
// leaks into it, breaking the `squashfs` case match. The path form keeps the
// suffix clean AND carries arch; bootFile() resolves /boot/<arch>/<name>
// directly. console argument differs by arch:
// x86 serial-over-LAN is ttyS0 (8250); arm64 servers' BMCs expose a PL011
// UART (ttyAMA0). The ttyS0 rationale comment above still applies on arm64
// — we keep the same serial-only strategy, just the right device.
const ipxeTemplate = `#!ipxe
kernel http://{{.ServerIP}}{{.HTTPAddr}}/boot/{{.Arch}}/vmlinuz initrd=initrd.img boot=live fetch=http://{{.ServerIP}}{{.HTTPAddr}}/boot/{{.Arch}}/filesystem.squashfs ip=dhcp ethdevice-timeout=90 console={{.Console}},115200 metalkit.url=http://{{.ServerIP}}{{.HTTPAddr}} systemd.mask=lvm2-pvscan@.service
initrd http://{{.ServerIP}}{{.HTTPAddr}}/boot/{{.Arch}}/initrd.img
boot
`

type ipxeVars struct {
	ServerIP string
	HTTPAddr string // ":PORT"
	Arch     string // "amd64" | "arm64"
	Console  string // serial console device matching the arch
}

// renderIPXE returns the rendered iPXE script body.
func renderIPXE(serverIP, httpAddr, arch string) (string, error) {
	if arch == "" {
		arch = "amd64"
	}
	console := "ttyS0"
	if arch == "arm64" {
		console = "ttyAMA0"
	}
	tpl, err := template.New("ipxe").Parse(ipxeTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, ipxeVars{ServerIP: serverIP, HTTPAddr: httpAddr, Arch: arch, Console: console}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
