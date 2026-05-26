package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// BSDPServer implements the PXE Boot Server Discovery (Layer 3) responder on
// UDP 4011. Strict PXE ROMs (Dell PowerEdge, some HP) ignore bootfile/siaddr
// in the port-67 OFFER and instead unicast a DHCP Request to UDP 4011 after
// obtaining an IP. The reply on 4011 carries siaddr, sname, and the bootfile
// so the client can begin TFTP.
//
// We use stdlib net.ListenUDP rather than insomniacslk/dhcp/server4 because
// server4 hardcodes port-67 semantics (giaddr handling, broadcast behavior)
// which do not apply on 4011.
type BSDPServer struct {
	cfg    Config
	logger *slog.Logger
	srvIP  net.IP
	listen *net.UDPAddr
	conn   *net.UDPConn
}

func NewBSDP(cfg Config) (*BSDPServer, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ip := net.ParseIP(cfg.ServerIP)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("invalid ServerIP %q", cfg.ServerIP)
	}
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":4011"
	}
	laddr, err := resolveListen(addr)
	if err != nil {
		return nil, err
	}
	return &BSDPServer{
		cfg:    cfg,
		logger: cfg.Logger,
		srvIP:  ip.To4(),
		listen: laddr,
	}, nil
}

func (s *BSDPServer) Start(ctx context.Context) error {
	conn, err := net.ListenUDP("udp4", s.listen)
	if err != nil {
		return fmt.Errorf("bsdp listen: %w", err)
	}
	s.conn = conn
	s.logger.Info("bsdp: listening", "addr", s.listen.String(), "server_ip", s.cfg.ServerIP)

	errCh := make(chan error, 1)
	go func() { errCh <- s.serve() }()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		<-errCh
		s.logger.Info("bsdp: stopped")
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("bsdp serve: %w", err)
		}
		return nil
	}
}

func (s *BSDPServer) serve() error {
	buf := make([]byte, 1500)
	for {
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if isClosedConnErr(err) {
				return nil
			}
			return err
		}
		req, perr := dhcpv4.FromBytes(buf[:n])
		if perr != nil {
			s.logger.Warn("bsdp: parse failed", "err", perr, "src", src.String())
			continue
		}
		s.logger.Info("bsdp: received",
			"mac", req.ClientHWAddr.String(),
			"xid", req.TransactionID.String(),
			"msg_type", req.MessageType().String(),
			"src", src.String(),
		)
		s.handle(req, src)
	}
}

func isClosedConnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "use of closed network connection")
}

func (s *BSDPServer) handle(req *dhcpv4.DHCPv4, src *net.UDPAddr) {
	reply, err := buildBSDPReply(req, s.srvIP)
	if err != nil {
		s.logger.Warn("bsdp: build reply failed", "err", err, "xid", req.TransactionID.String(), "src", src.String())
		return
	}
	if reply == nil {
		return
	}

	archHex := ""
	if archs := req.ClientArch(); len(archs) > 0 {
		archHex = fmt.Sprintf("0x%04x", uint16(archs[0]))
	}
	cidHex := ""
	if cid := req.GetOneOption(dhcpv4.OptionClientMachineIdentifier); len(cid) > 0 {
		cidHex = fmt.Sprintf("%x", cid)
	}
	s.logger.Info("bsdp: reply",
		"stage", "bsdp",
		"mac", req.ClientHWAddr.String(),
		"arch", archHex,
		"bootfile", reply.BootFileName,
		"client_id", cidHex,
		"xid", req.TransactionID.String(),
		"dest", src.String(),
	)

	if _, err := s.conn.WriteToUDP(reply.ToBytes(), src); err != nil {
		s.logger.Warn("bsdp: write failed", "err", err, "dest", src.String())
	}
}

// buildBSDPReply constructs the UDP 4011 ACK. The wire shape mirrors what
// dnsmasq emits in proxyDHCP mode (verified to boot Dell PowerEdge BIOS):
//   - msg-type = ACK (not Offer — strict PXE ROMs accept a single-step ACK on 4011)
//   - server-id (54) = serverIP
//   - class-id (60) = "PXEClient"
//   - GUID (97) echoed
//   - siaddr = serverIP (this is the TFTP server the client will contact)
//   - sname = serverIP
//   - file = bootfile chosen by arch
func buildBSDPReply(req *dhcpv4.DHCPv4, srvIP net.IP) (*dhcpv4.DHCPv4, error) {
	if req == nil {
		return nil, nil
	}
	// iPXE never speaks BSDP — it always does Layer 1 on port 67.
	if isIPXE(req) {
		return nil, nil
	}
	cid := req.ClassIdentifier()
	if !strings.HasPrefix(cid, "PXEClient") {
		return nil, nil
	}
	bf, ok := selectBootfile(req)
	if !ok {
		return nil, nil
	}

	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithServerIP(srvIP),
	)
	if err != nil {
		return nil, err
	}
	reply.YourIPAddr = req.ClientIPAddr
	reply.ServerIPAddr = srvIP
	reply.Flags = req.Flags
	reply.BootFileName = bf
	reply.ServerHostName = srvIP.String()

	reply.UpdateOption(dhcpv4.OptServerIdentifier(srvIP))
	reply.UpdateOption(dhcpv4.OptClassIdentifier("PXEClient"))
	if uuid := req.GetOneOption(dhcpv4.OptionClientMachineIdentifier); len(uuid) > 0 {
		reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientMachineIdentifier, uuid))
	}
	return reply, nil
}
