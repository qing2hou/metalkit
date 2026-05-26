package dhcp

import (
	"bytes"
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

func testCfg() *Config {
	return &Config{
		Interface:  "lo",
		ListenAddr: ":67",
		ServerIP:   "10.99.0.1",
		HTTPURL:    "http://10.99.0.1:8080/boot/ipxe",
	}
}

func mustReq(t *testing.T, mt dhcpv4.MessageType, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	mac, _ := net.ParseMAC("00:11:22:33:44:55")
	base := []dhcpv4.Modifier{
		dhcpv4.WithHwAddr(mac),
		dhcpv4.WithMessageType(mt),
	}
	base = append(base, mods...)
	p, err := dhcpv4.New(base...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestBuildReply_NonPXEDropped(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeDiscover)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil reply for non-PXE client, got %v", reply.Summary())
	}
}

func TestBuildReply_FirstStageBIOS(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00000:UNDI:002001")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0000))),
	)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply, got nil")
	}
	// Port-67 first-stage OFFER: no bootfile (delivered via BSDP on 4011),
	// siaddr = srvIP so the UEFI PXE ROM knows where to unicast BSDP REQUEST.
	// Dell BIOS ignores siaddr; Dell UEFI (arch 0x0007) requires it.
	if reply.BootFileName != "" {
		t.Errorf("BootFileName = %q, want empty (delivered via BSDP)", reply.BootFileName)
	}
	if reply.ServerHostName != "" {
		t.Errorf("ServerHostName = %q, want empty", reply.ServerHostName)
	}
	if !reply.ServerIPAddr.Equal(net.IPv4(10, 99, 0, 1)) {
		t.Errorf("ServerIPAddr = %v, want 10.99.0.1 (siaddr set for BSDP follow-up)", reply.ServerIPAddr)
	}
	if got := reply.GetOneOption(dhcpv4.OptionTFTPServerName); len(got) != 0 {
		t.Errorf("option 66 = %q, want absent", got)
	}
	if got := reply.GetOneOption(dhcpv4.OptionVendorSpecificInformation); len(got) != 0 {
		t.Errorf("option 43 = %x, want absent", got)
	}
	if got := reply.ClassIdentifier(); got != "PXEClient" {
		t.Errorf("ClassIdentifier = %q, want PXEClient", got)
	}
	if !reply.YourIPAddr.Equal(net.IPv4zero) {
		t.Errorf("YourIPAddr = %v, want 0.0.0.0", reply.YourIPAddr)
	}
	if reply.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("MessageType = %v, want Offer", reply.MessageType())
	}
	if !reply.ServerIdentifier().Equal(net.IPv4(10, 99, 0, 1)) {
		t.Errorf("ServerIdentifier = %v", reply.ServerIdentifier())
	}
}

func TestBuildReply_FirstStageUEFIx64(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
	)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply for known arch, got nil")
	}
	if reply.BootFileName != "" {
		t.Errorf("BootFileName = %q, want empty", reply.BootFileName)
	}
}

func TestBuildReply_FirstStageUEFIx64Alt(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0009))),
	)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply for known arch, got nil")
	}
}

func TestBuildReply_UnknownArchDrop(t *testing.T) {
	req := mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x00ff))),
	)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil reply for unknown arch, got %v", reply.Summary())
	}
}

func TestBuildReply_BIOSRequestDropped(t *testing.T) {
	// Pre-iPXE BIOS DHCPRequest must not get an ACK from us — otherwise a
	// coexisting full DHCP (dnsmasq) and our proxy both ACK with the same
	// Server-ID but different yiaddr, and the client DECLINEs.
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
	)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != nil {
		t.Fatalf("expected nil for BIOS Request, got %v", reply.Summary())
	}
}

func TestBuildReply_SecondStageIPXE(t *testing.T) {
	cfg := testCfg()
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
		dhcpv4.WithUserClass("iPXE", false),
	)
	reply, err := buildReply(req, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply")
	}
	if reply.BootFileName != cfg.HTTPURL {
		t.Errorf("BootFileName = %q, want %q", reply.BootFileName, cfg.HTTPURL)
	}
	if reply.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("MessageType = %v, want Ack", reply.MessageType())
	}
	if reply.ServerHostName != "" {
		t.Errorf("ServerHostName = %q, want empty", reply.ServerHostName)
	}
}

func TestBuildReply_UUIDEcho(t *testing.T) {
	uuid := []byte{0x00, 0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}
	req := mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
		dhcpv4.WithOption(dhcpv4.OptGeneric(dhcpv4.OptionClientMachineIdentifier, uuid)),
	)
	reply, err := buildReply(req, testCfg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply")
	}
	got := reply.GetOneOption(dhcpv4.OptionClientMachineIdentifier)
	if !bytes.Equal(got, uuid) {
		t.Errorf("option 97 = %x, want %x", got, uuid)
	}
}

func TestBootfileForArch(t *testing.T) {
	cases := []struct {
		a    ClientArch
		want string
		ok   bool
	}{
		{ArchBIOSx86, "undionly.kpxe", true},
		{ArchUEFIIA32, "ipxe.efi", true},
		{ArchUEFIx64, "snponly.efi", true},
		{ArchUEFIx64Alt, "snponly.efi", true},
		{ArchUEFIArm64, "arm64-snponly.efi", true},
		{ClientArch(0x00ff), "", false},
	}
	for _, c := range cases {
		got, ok := bootfileForArch(c.a)
		if got != c.want || ok != c.ok {
			t.Errorf("bootfileForArch(%v) = (%q, %v), want (%q, %v)", c.a, got, ok, c.want, c.ok)
		}
	}
}

func TestClientArchString(t *testing.T) {
	if ArchBIOSx86.String() != "bios-x86" {
		t.Errorf("got %q", ArchBIOSx86.String())
	}
	if ClientArch(0xffff).String() != "unknown" {
		t.Errorf("got %q", ClientArch(0xffff).String())
	}
}
