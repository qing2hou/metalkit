package dhcp

import (
	"bytes"
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

var bsdpSrv = net.IPv4(10, 99, 0, 1).To4()

func TestBuildBSDPReply_NonPXEDropped(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil for non-PXE client, got %v", reply.Summary())
	}
}

func TestBuildBSDPReply_NoArchDropped(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil when no arch present, got %v", reply.Summary())
	}
}

func TestBuildBSDPReply_IPXEDropped(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
		dhcpv4.WithUserClass("iPXE", false),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil for iPXE second-stage on 4011, got %v", reply.Summary())
	}
}

func TestBuildBSDPReply_BIOS(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00000:UNDI:002001")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0000))),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply")
	}
	if reply.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("MessageType = %v, want Ack", reply.MessageType())
	}
	if reply.BootFileName != "undionly.kpxe" {
		t.Errorf("BootFileName = %q, want undionly.kpxe", reply.BootFileName)
	}
	if !reply.ServerIPAddr.Equal(bsdpSrv) {
		t.Errorf("ServerIPAddr (siaddr) = %v, want %v", reply.ServerIPAddr, bsdpSrv)
	}
	if reply.ServerHostName != bsdpSrv.String() {
		t.Errorf("ServerHostName = %q, want %q", reply.ServerHostName, bsdpSrv.String())
	}
	if got := reply.ClassIdentifier(); got != "PXEClient" {
		t.Errorf("ClassIdentifier = %q, want PXEClient", got)
	}
	if !reply.ServerIdentifier().Equal(bsdpSrv) {
		t.Errorf("ServerIdentifier = %v, want %v", reply.ServerIdentifier(), bsdpSrv)
	}
}

func TestBuildBSDPReply_UEFIx64(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil || reply.BootFileName != "snponly.efi" {
		t.Fatalf("got %v, want snponly.efi", reply)
	}
}

func TestBuildBSDPReply_UnknownArchDropped(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x00ff))),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil for unknown arch, got %v", reply.Summary())
	}
}

func TestBuildBSDPReply_UUIDEcho(t *testing.T) {
	uuid := []byte{0x00, 0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
		dhcpv4.WithOption(dhcpv4.OptGeneric(dhcpv4.OptionClientMachineIdentifier, uuid)),
	)
	reply, err := buildBSDPReply(req, bsdpSrv)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply")
	}
	if got := reply.GetOneOption(dhcpv4.OptionClientMachineIdentifier); !bytes.Equal(got, uuid) {
		t.Errorf("option 97 = %x, want %x", got, uuid)
	}
}

func TestSelectBootfile(t *testing.T) {
	if _, ok := selectBootfile(nil); ok {
		t.Error("nil req should return ok=false")
	}
	req := mustReq(t, dhcpv4.MessageTypeDiscover)
	if _, ok := selectBootfile(req); ok {
		t.Error("missing arch should return ok=false")
	}
	req = mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
	)
	bf, ok := selectBootfile(req)
	if !ok || bf != "snponly.efi" {
		t.Errorf("selectBootfile = (%q, %v), want (snponly.efi, true)", bf, ok)
	}
}
