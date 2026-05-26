package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

type Config struct {
	Interface  string
	ListenAddr string
	ServerIP   string
	HTTPURL    string
	Logger     *slog.Logger
}

type Server struct {
	cfg     Config
	srv     *server4.Server
	logger  *slog.Logger
	srvIP   net.IP
	listen  *net.UDPAddr
}

func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ip := net.ParseIP(cfg.ServerIP)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("invalid ServerIP %q", cfg.ServerIP)
	}
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":67"
	}
	laddr, err := resolveListen(addr)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		logger: cfg.Logger,
		srvIP:  ip.To4(),
		listen: laddr,
	}, nil
}

func resolveListen(addr string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid ListenAddr %q: %w", addr, err)
	}
	ip := net.IPv4zero
	if host != "" {
		ip = net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("invalid host in ListenAddr %q", addr)
		}
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		return nil, fmt.Errorf("invalid port in ListenAddr %q: %w", addr, err)
	}
	return &net.UDPAddr{IP: ip, Port: p}, nil
}

func (s *Server) Start(ctx context.Context) error {
	srv, err := server4.NewServer(s.cfg.Interface, s.listen, s.handler,
		server4.WithLogger(slogLogger{s.logger}))
	if err != nil {
		return fmt.Errorf("dhcp listen: %w", err)
	}
	s.srv = srv
	s.logger.Info("dhcp: listening", "iface", s.cfg.Interface, "addr", s.listen.String(), "server_ip", s.cfg.ServerIP)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()

	select {
	case <-ctx.Done():
		_ = srv.Close()
		<-errCh
		s.logger.Info("dhcp: stopped")
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("dhcp serve: %w", err)
		}
		return nil
	}
}

func (s *Server) handler(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	s.logger.Info("dhcp: received",
		"mac", req.ClientHWAddr.String(),
		"xid", req.TransactionID.String(),
		"msg_type", req.MessageType().String(),
	)
	reply, err := buildReply(req, &s.cfg)
	if err != nil {
		s.logger.Warn("dhcp: build reply failed", "err", err, "xid", req.TransactionID.String())
		return
	}
	if reply == nil {
		return
	}

	stage := "first"
	bf := reply.BootFileName
	if isIPXE(req) {
		stage = "ipxe"
	}
	archHex := ""
	if archs := req.ClientArch(); len(archs) > 0 {
		archHex = fmt.Sprintf("0x%04x", uint16(archs[0]))
	}
	cidHex := ""
	if cid := req.GetOneOption(dhcpv4.OptionClientMachineIdentifier); len(cid) > 0 {
		cidHex = fmt.Sprintf("%x", cid)
	}
	s.logger.Info("dhcp: reply",
		"stage", stage,
		"mac", req.ClientHWAddr.String(),
		"arch", archHex,
		"bootfile", bf,
		"client_id", cidHex,
		"xid", req.TransactionID.String(),
	)

	dest := destFor(req, peer)
	if _, err := conn.WriteTo(reply.ToBytes(), dest); err != nil {
		s.logger.Warn("dhcp: write failed", "err", err, "dest", dest.String())
	}
}

func destFor(req *dhcpv4.DHCPv4, peer net.Addr) net.Addr {
	if req.IsBroadcast() || req.GatewayIPAddr.IsUnspecified() && (peer == nil || isZeroPeer(peer)) {
		return &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
	}
	if !req.GatewayIPAddr.IsUnspecified() {
		return &net.UDPAddr{IP: req.GatewayIPAddr, Port: 67}
	}
	return peer
}

func isZeroPeer(peer net.Addr) bool {
	u, ok := peer.(*net.UDPAddr)
	if !ok {
		return false
	}
	return u.IP == nil || u.IP.IsUnspecified()
}

// slogLogger adapts slog to server4.Logger.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Printf(format string, v ...interface{}) {
	s.l.Warn("dhcp: server4", "msg", fmt.Sprintf(format, v...))
}
func (s slogLogger) PrintMessage(prefix string, message *dhcpv4.DHCPv4) {
	s.l.Info("dhcp: server4", "prefix", prefix, "summary", message.Summary())
}

func isIPXE(req *dhcpv4.DHCPv4) bool {
	for _, uc := range req.UserClass() {
		if uc == "iPXE" {
			return true
		}
	}
	return false
}

func buildReply(req *dhcpv4.DHCPv4, cfg *Config) (*dhcpv4.DHCPv4, error) {
	if req == nil {
		return nil, nil
	}
	cid := req.ClassIdentifier()
	if !strings.HasPrefix(cid, "PXEClient") {
		return nil, nil
	}

	var msgType dhcpv4.MessageType
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		msgType = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		// We reply ACK to a Request only when the client is iPXE running its
		// second-stage DHCP (option 77 = "iPXE"). For the pre-iPXE BIOS PXE
		// stage we MUST NOT reply to the Request: doing so creates a second
		// ACK with yiaddr=0 alongside the real DHCP server's ACK, and strict
		// clients (Dell PowerEdge) then DECLINE the offered lease because the
		// two ACKs disagree on yiaddr. Our port-67 role for BIOS is purely an
		// advertisement on Discover; bootfile delivery happens via BSDP on
		// UDP 4011.
		if !isIPXE(req) {
			return nil, nil
		}
		msgType = dhcpv4.MessageTypeAck
	default:
		return nil, nil
	}

	srvIP := net.ParseIP(cfg.ServerIP).To4()
	if srvIP == nil {
		return nil, fmt.Errorf("invalid ServerIP %q", cfg.ServerIP)
	}

	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(msgType),
		dhcpv4.WithServerIP(srvIP),
	)
	if err != nil {
		return nil, err
	}
	reply.YourIPAddr = net.IPv4zero
	reply.ServerIPAddr = srvIP
	reply.Flags = req.Flags

	reply.UpdateOption(dhcpv4.OptServerIdentifier(srvIP))
	reply.UpdateOption(dhcpv4.OptClassIdentifier("PXEClient"))

	if uuid := req.GetOneOption(dhcpv4.OptionClientMachineIdentifier); len(uuid) > 0 {
		reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientMachineIdentifier, uuid))
	}

	if isIPXE(req) {
		reply.BootFileName = cfg.HTTPURL
		reply.ServerHostName = ""
		return reply, nil
	}

	// Port-67 first-stage OFFER (pre-iPXE): minimal advertisement.
	// We MUST NOT put a bootfile here — strict PXE ROMs (Dell PowerEdge)
	// reject the OFFER with PXE-E16 when bootfile is set. Instead the client
	// discovers the bootfile via BSDP on UDP 4011 (see bsdp.go).
	// siaddr (ServerIPAddr) is left set to srvIP so the UEFI PXE ROM knows
	// where to unicast its BSDP REQUEST. Dell BIOS ignores this field, but
	// Dell UEFI (arch 0x0007) requires it.
	if _, ok := selectBootfile(req); !ok {
		return nil, nil
	}
	reply.BootFileName = ""
	reply.ServerHostName = ""
	return reply, nil
}
