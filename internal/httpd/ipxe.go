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
const ipxeTemplate = `#!ipxe
kernel http://{{.ServerIP}}{{.HTTPAddr}}/boot/vmlinuz initrd=initrd.img boot=live fetch=http://{{.ServerIP}}{{.HTTPAddr}}/boot/filesystem.squashfs ip=dhcp console=ttyS0,115200 metalkit.url=http://{{.ServerIP}}{{.HTTPAddr}}
initrd http://{{.ServerIP}}{{.HTTPAddr}}/boot/initrd.img
boot
`

type ipxeVars struct {
	ServerIP string
	HTTPAddr string // ":PORT"
}

// renderIPXE returns the rendered iPXE script body.
func renderIPXE(serverIP, httpAddr string) (string, error) {
	tpl, err := template.New("ipxe").Parse(ipxeTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, ipxeVars{ServerIP: serverIP, HTTPAddr: httpAddr}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
