package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

// Mode toggles between ProxyDHCP (PXE-only, expects external DHCP server)
// and Full (we hand out IP leases ourselves). Empty defaults to Proxy.
type Mode string

const (
	ModeProxy Mode = "proxy"
	ModeFull  Mode = "full"
)

type Config struct {
	Interface  string
	ListenAddr string
	ServerIP   string
	HTTPURL    string
	Logger     *slog.Logger

	// Full-mode-only fields. When Mode == ModeProxy these are ignored.
	Mode   Mode
	Pool   *Pool
	Leases LeaseStore
}

// LeaseStore is the slice of internal/leases the DHCP handler needs.
// Defined here (instead of importing leases) to keep this package
// dependency-free at the protocol layer and to ease testing with fakes.
type LeaseStore interface {
	Allocate(ctx context.Context, in AllocateInput) (allocated string, err error)
	// Confirm returns the IP the store actually confirmed for this MAC. The
	// DHCP layer fills the ACK's yiaddr with that value — required for iPXE's
	// second-stage REQUEST, which on some NICs sends neither option 50 nor
	// ciaddr. Without a server-side lookup the ACK would carry yiaddr=0 and
	// iPXE would report "No configuration methods succeeded".
	Confirm(ctx context.Context, mac, requestedIP string, leaseDur time.Duration) (confirmedIP string, err error)
	Release(ctx context.Context, mac string) error
}

// AllocateInput mirrors leases.AllocateInput but trimmed to what the
// protocol layer needs. The leases adapter in cmd/controller bridges the
// two structs.
type AllocateInput struct {
	MAC      string
	Hostname string
	Start    netip.Addr
	End      netip.Addr
	Exclude  map[string]struct{}
	LeaseDur time.Duration
}

type Server struct {
	cfg    Config
	srv    *server4.Server
	logger *slog.Logger
	srvIP  net.IP
	listen *net.UDPAddr

	// mu guards the hot-reloadable fields (mode, pool, leases). UDP/67 stays
	// bound the whole time — Reload only swaps the in-memory references, so
	// the socket never closes and PXE clients never see a window of "no
	// DHCP server on the wire". Each handler invocation grabs an RLock for
	// the duration of one packet, which costs ~nothing under contention.
	mu     sync.RWMutex
	mode   Mode
	pool   *Pool
	leases LeaseStore
}

func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeProxy
	}
	if cfg.Mode == ModeFull {
		if cfg.Pool == nil {
			return nil, fmt.Errorf("full mode requires Pool")
		}
		if cfg.Leases == nil {
			return nil, fmt.Errorf("full mode requires Leases store")
		}
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
		mode:   cfg.Mode,
		pool:   cfg.Pool,
		leases: cfg.Leases,
	}, nil
}

// Reload atomically swaps the runtime fields the handler reads on every
// packet. The UDP socket stays bound — only the policy (mode, pool, lease
// store) changes. Safe to call concurrently with in-flight DHCP exchanges:
// any handler currently mid-packet finishes with the old values, the next
// packet sees the new ones.
//
// Validation here mirrors New(): ModeFull requires both a Pool and a
// LeaseStore. An invalid combination is rejected before any state is
// touched, so a bad call is a no-op.
func (s *Server) Reload(mode Mode, pool *Pool, leases LeaseStore) error {
	if mode == "" {
		mode = ModeProxy
	}
	if mode != ModeProxy && mode != ModeFull {
		return fmt.Errorf("invalid mode %q", mode)
	}
	if mode == ModeFull {
		if pool == nil {
			return fmt.Errorf("full mode requires Pool")
		}
		if leases == nil {
			return fmt.Errorf("full mode requires Leases store")
		}
	}
	s.mu.Lock()
	s.mode = mode
	s.pool = pool
	s.leases = leases
	s.mu.Unlock()
	s.logger.Info("dhcp: reloaded", "mode", string(mode))
	return nil
}

// snapshot returns the current mode/pool/leases under read lock — handlers
// call this once at the top of each packet so the rest of the per-packet
// work uses a consistent view even if Reload fires mid-handler.
func (s *Server) snapshot() (Mode, *Pool, LeaseStore) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode, s.pool, s.leases
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
	srv, err := server4.NewServer(s.cfg.Interface, s.listen, s.makeHandler(ctx),
		server4.WithLogger(slogLogger{s.logger}))
	if err != nil {
		return fmt.Errorf("dhcp listen: %w", err)
	}
	s.srv = srv
	s.logger.Info("dhcp: listening",
		"iface", s.cfg.Interface,
		"addr", s.listen.String(),
		"server_ip", s.cfg.ServerIP,
		"mode", string(s.mode),
	)

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

// makeHandler returns a server4 handler closed over the run ctx so leases
// calls inherit cancellation if the server is stopping.
func (s *Server) makeHandler(ctx context.Context) server4.Handler {
	return func(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
		s.handle(ctx, conn, peer, req)
	}
}

func (s *Server) handle(ctx context.Context, conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	s.logger.Info("dhcp: received",
		"mac", req.ClientHWAddr.String(),
		"xid", req.TransactionID.String(),
		"msg_type", req.MessageType().String(),
	)

	mode, pool, leases := s.snapshot()

	var reply *dhcpv4.DHCPv4
	var err error
	if mode == ModeFull {
		reply, err = s.buildFullReplyWith(ctx, req, pool, leases)
	} else {
		reply, err = buildReply(req, &s.cfg)
	}
	if err != nil {
		s.logger.Warn("dhcp: build reply failed", "err", err, "xid", req.TransactionID.String())
		return
	}
	if reply == nil {
		return
	}

	stage := "first"
	if isIPXE(req) {
		stage = "ipxe"
	} else if mode == ModeFull {
		stage = "lease"
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
		"msg_type", reply.MessageType().String(),
		"yiaddr", reply.YourIPAddr.String(),
		"bootfile", reply.BootFileName,
		"client_id", cidHex,
		"xid", req.TransactionID.String(),
	)

	dest := destFor(req, peer)
	if _, err := conn.WriteTo(reply.ToBytes(), dest); err != nil {
		s.logger.Warn("dhcp: write failed", "err", err, "dest", dest.String())
	}
}

// buildFullReply handles DISCOVER/REQUEST/RELEASE/DECLINE in full mode.
// PXEClient handshakes layer their bootfile fields on top of the standard
// lease options so a single OFFER both gives the client an IP AND tells it
// where to TFTP from.
//
// This 2-arg form is kept for tests; production code calls
// buildFullReplyWith() so a single packet uses a consistent snapshot of
// the reloadable state.
func (s *Server) buildFullReply(ctx context.Context, req *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	_, pool, leases := s.snapshot()
	return s.buildFullReplyWith(ctx, req, pool, leases)
}

func (s *Server) buildFullReplyWith(ctx context.Context, req *dhcpv4.DHCPv4, pool *Pool, leases LeaseStore) (*dhcpv4.DHCPv4, error) {
	switch req.MessageType() {
	case dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline:
		_ = leases.Release(ctx, req.ClientHWAddr.String())
		return nil, nil
	case dhcpv4.MessageTypeDiscover:
		return s.buildOffer(ctx, req, pool, leases)
	case dhcpv4.MessageTypeRequest:
		return s.buildAck(ctx, req, pool, leases)
	default:
		return nil, nil
	}
}

func (s *Server) buildOffer(ctx context.Context, req *dhcpv4.DHCPv4, pool *Pool, leases LeaseStore) (*dhcpv4.DHCPv4, error) {
	mac := req.ClientHWAddr.String()
	in := AllocateInput{
		MAC:      mac,
		Hostname: req.HostName(),
		Start:    pool.Start,
		End:      pool.End,
		Exclude:  pool.Exclude,
		LeaseDur: time.Duration(pool.LeaseSec) * time.Second,
	}
	ipStr, err := leases.Allocate(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("allocate lease: %w", err)
	}
	yi, err := netip.ParseAddr(ipStr)
	if err != nil {
		return nil, fmt.Errorf("parse allocated %q: %w", ipStr, err)
	}

	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
		dhcpv4.WithServerIP(s.srvIP),
	)
	if err != nil {
		return nil, err
	}
	reply.YourIPAddr = toIPv4(yi)
	reply.ServerIPAddr = s.srvIP
	reply.Flags = req.Flags
	reply.UpdateOption(dhcpv4.OptServerIdentifier(s.srvIP))

	applyLeaseOptions(reply, pool)
	s.maybeAttachPXE(req, reply)
	return reply, nil
}

func (s *Server) buildAck(ctx context.Context, req *dhcpv4.DHCPv4, pool *Pool, leases LeaseStore) (*dhcpv4.DHCPv4, error) {
	mac := req.ClientHWAddr.String()
	// Requested IP comes from option 50 (preferred) or ciaddr (renewing client).
	requested := ""
	if v := req.GetOneOption(dhcpv4.OptionRequestedIPAddress); len(v) == 4 {
		requested = net.IPv4(v[0], v[1], v[2], v[3]).String()
	} else if !req.ClientIPAddr.IsUnspecified() && req.ClientIPAddr.To4() != nil {
		requested = req.ClientIPAddr.String()
	}
	if requested != "" {
		ra, err := netip.ParseAddr(requested)
		if err != nil || !pool.Contains(ra) {
			return buildNak(req, s.srvIP, "requested IP outside pool")
		}
	}

	leaseDur := time.Duration(pool.LeaseSec) * time.Second
	confirmed, err := leases.Confirm(ctx, mac, requested, leaseDur)
	if err != nil {
		return buildNak(req, s.srvIP, err.Error())
	}

	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithServerIP(s.srvIP),
	)
	if err != nil {
		return nil, err
	}
	// Prefer the requested IP when present so first-stage PXE clients see
	// exactly what they asked for. Fall back to the store's confirmed IP for
	// iPXE's second-stage REQUEST, which may carry neither option 50 nor a
	// ciaddr — that's the path that left yiaddr=0 in the wild and broke PXE.
	if requested != "" {
		reply.YourIPAddr = net.ParseIP(requested).To4()
	} else if confirmed != "" {
		reply.YourIPAddr = net.ParseIP(confirmed).To4()
	}
	reply.ServerIPAddr = s.srvIP
	reply.Flags = req.Flags
	reply.UpdateOption(dhcpv4.OptServerIdentifier(s.srvIP))
	applyLeaseOptions(reply, pool)
	s.maybeAttachPXE(req, reply)
	return reply, nil
}

// buildNak constructs a DHCPNAK for an invalid REQUEST. We send NAK rather
// than silently dropping so the client immediately restarts DISCOVER instead
// of timing out for ~30 s.
func buildNak(req *dhcpv4.DHCPv4, srvIP net.IP, reason string) (*dhcpv4.DHCPv4, error) {
	reply, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeNak),
		dhcpv4.WithServerIP(srvIP),
	)
	if err != nil {
		return nil, err
	}
	reply.YourIPAddr = net.IPv4zero
	reply.UpdateOption(dhcpv4.OptServerIdentifier(srvIP))
	reply.UpdateOption(dhcpv4.OptMessage(reason))
	return reply, nil
}

// maybeAttachPXE layers the PXE boot-info fields onto a full-mode reply
// when the request is from a PXEClient. Two paths:
//   - First-stage (BIOS or UEFI ROM, not yet iPXE): set ServerIPAddr/sname
//     and the bootfile selected by arch. Unlike proxy mode we DO set
//     BootFileName here because we're the only DHCP on the wire — there's
//     no risk of disagreeing with another server's yiaddr.
//   - Second-stage (iPXE): set BootFileName to the HTTP iPXE script URL.
func (s *Server) maybeAttachPXE(req, reply *dhcpv4.DHCPv4) {
	cid := req.ClassIdentifier()
	if !strings.HasPrefix(cid, "PXEClient") {
		return
	}
	reply.UpdateOption(dhcpv4.OptClassIdentifier("PXEClient"))
	if uuid := req.GetOneOption(dhcpv4.OptionClientMachineIdentifier); len(uuid) > 0 {
		reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientMachineIdentifier, uuid))
	}
	if isIPXE(req) {
		reply.BootFileName = s.cfg.HTTPURL
		reply.ServerHostName = ""
		return
	}
	bf, ok := selectBootfile(req)
	if !ok {
		return
	}
	reply.BootFileName = bf
	reply.ServerHostName = s.cfg.ServerIP
	reply.ServerIPAddr = s.srvIP
}

func (s *Server) handler(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	// retained for tests that call the legacy entry point; new code uses
	// makeHandler/handle via Start().
	s.handle(context.Background(), conn, peer, req)
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

// buildReply is the legacy ProxyDHCP path: PXEClient-only, no IP allocation.
// Kept verbatim from the pre-full-mode implementation so upgrades that
// stay in proxy mode see bit-for-bit identical wire behaviour.
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
