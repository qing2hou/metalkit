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
kernel http://{{.ServerIP}}{{.HTTPAddr}}/boot/{{.Arch}}/vmlinuz initrd=initrd.img boot=live fetch=http://{{.ServerIP}}{{.HTTPAddr}}/boot/{{.Arch}}/filesystem.squashfs ip=dhcp console={{.Console}},115200 metalkit.url=http://{{.ServerIP}}{{.HTTPAddr}}
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
