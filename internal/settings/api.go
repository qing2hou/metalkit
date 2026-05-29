package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"metalkit/internal/config"
)

// Setting keys used for the DHCP feature. Keep these in one place so the
// store, API, and main.go overlay code can't drift.
const (
	KeyDHCPMode       = "dhcp.mode"
	KeyDHCPStart      = "dhcp.pool.start"
	KeyDHCPEnd        = "dhcp.pool.end"
	KeyDHCPNetmask    = "dhcp.pool.netmask"
	KeyDHCPGateway    = "dhcp.pool.gateway"
	KeyDHCPDNS        = "dhcp.pool.dns"         // comma-separated
	KeyDHCPLeaseHours = "dhcp.pool.lease_hours" // integer string
	KeyDHCPExclude    = "dhcp.pool.exclude"     // comma-separated
)

// DHCPSettings is the GET response and the PUT request shape.
// Sent to the browser; the UI form maps 1:1 to these fields.
type DHCPSettings struct {
	Mode       string   `json:"mode"`
	Start      string   `json:"start"`
	End        string   `json:"end"`
	Netmask    string   `json:"netmask"`
	Gateway    string   `json:"gateway"`
	DNS        []string `json:"dns"`
	LeaseHours int      `json:"lease_hours"`
	Exclude    []string `json:"exclude"`
}

// DHCPSettingsResponse layers a "restart required" hint over the bare
// settings so the UI can show a banner after PUT — the DHCP server can't
// hot-reload, so changes take effect only after the controller restarts.
type DHCPSettingsResponse struct {
	DHCPSettings
	RestartRequired bool `json:"restart_required"`
}

// API exposes the DHCP settings endpoints. The bootCfg field holds the
// values loaded from config.yaml at process start; the store overlays
// runtime overrides on top. GET returns the merged effective settings.
type API struct {
	store    *Store
	logger   *slog.Logger
	bootCfg  *config.Config // for serverIP + interface, used in PUT validation
	reloader DHCPReloader   // optional hot-reload callback
}

// DHCPReloader is the contract main.go fulfils so the settings API can
// hot-reload the DHCP server after a PUT. Implementations should:
//   - rebuild the dhcp.Pool from the new settings
//   - lazily create the leases store if mode flipped to full
//   - call dhcp.Server.Reload to swap the runtime policy in-place
//
// Returning a non-nil error makes the PUT response include
// restart_required:true so the UI nudges the operator to restart.
type DHCPReloader interface {
	ReloadDHCP(ctx context.Context, s DHCPSettings) error
}

func NewAPI(store *Store, bootCfg *config.Config, logger *slog.Logger) *API {
	return &API{store: store, bootCfg: bootCfg, logger: logger}
}

// WithReloader registers a hot-reload callback. Returns the API for
// chaining at wire-up time. Nil keeps the legacy restart-required UX —
// useful for tests, where we don't want to construct a full DHCP server.
func (a *API) WithReloader(r DHCPReloader) *API {
	a.reloader = r
	return a
}

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/settings/dhcp", a.getDHCP)
	mux.HandleFunc("PUT /api/v1/settings/dhcp", a.putDHCP)
}

func (a *API) getDHCP(w http.ResponseWriter, r *http.Request) {
	s, err := a.effectiveDHCP(r.Context())
	if err != nil {
		a.logger.Error("settings get dhcp", "err", err)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) putDHCP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var in DHCPSettings
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if err := validateDHCPSettings(&in, a.bootCfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	kv := map[string]string{
		KeyDHCPMode:       in.Mode,
		KeyDHCPStart:      in.Start,
		KeyDHCPEnd:        in.End,
		KeyDHCPNetmask:    in.Netmask,
		KeyDHCPGateway:    in.Gateway,
		KeyDHCPDNS:        strings.Join(in.DNS, ","),
		KeyDHCPLeaseHours: strconv.Itoa(in.LeaseHours),
		KeyDHCPExclude:    strings.Join(in.Exclude, ","),
	}
	if err := a.store.SetMany(r.Context(), kv, basicAuthUser(r)); err != nil {
		a.logger.Error("settings put dhcp", "err", err)
		writeError(w, http.StatusInternalServerError, "save failed")
		return
	}
	a.logger.Info("dhcp settings updated", "user", basicAuthUser(r), "mode", in.Mode,
		"start", in.Start, "end", in.End)

	// Try to hot-reload the running DHCP server. If it succeeds, the UI can
	// drop the "请重启控制器" banner. If it fails — e.g. mode flipped to
	// full but interface auto-derivation explodes — we still kept the rows
	// in SQLite, so a manual restart will apply them anyway. We log the
	// failure and tell the UI to ask the operator to restart.
	restartRequired := true
	if a.reloader != nil {
		if err := a.reloader.ReloadDHCP(r.Context(), in); err != nil {
			a.logger.Warn("dhcp hot-reload failed; saved but restart required", "err", err)
		} else {
			restartRequired = false
		}
	}

	writeJSON(w, http.StatusOK, DHCPSettingsResponse{
		DHCPSettings:    in,
		RestartRequired: restartRequired,
	})
}

// effectiveDHCP merges the boot config values (loaded from config.yaml) with
// any overrides recorded in the settings table. This is the same merge
// applied at startup in main.go, so the UI shows exactly what the next
// controller restart would use.
func (a *API) effectiveDHCP(ctx context.Context) (DHCPSettings, error) {
	out := DHCPSettings{Mode: a.bootCfg.DHCPMode}
	if a.bootCfg.DHCPPool != nil {
		out.Start = a.bootCfg.DHCPPool.Start
		out.End = a.bootCfg.DHCPPool.End
		out.Netmask = a.bootCfg.DHCPPool.Netmask
		out.Gateway = a.bootCfg.DHCPPool.Gateway
		out.DNS = append([]string(nil), a.bootCfg.DHCPPool.DNS...)
		out.LeaseHours = a.bootCfg.DHCPPool.LeaseHours
		out.Exclude = append([]string(nil), a.bootCfg.DHCPPool.Exclude...)
	}

	overrides, err := a.store.GetMany(ctx, []string{
		KeyDHCPMode, KeyDHCPStart, KeyDHCPEnd, KeyDHCPNetmask, KeyDHCPGateway,
		KeyDHCPDNS, KeyDHCPLeaseHours, KeyDHCPExclude,
	})
	if err != nil {
		return out, err
	}
	if v, ok := overrides[KeyDHCPMode]; ok {
		out.Mode = v
	}
	if v, ok := overrides[KeyDHCPStart]; ok {
		out.Start = v
	}
	if v, ok := overrides[KeyDHCPEnd]; ok {
		out.End = v
	}
	if v, ok := overrides[KeyDHCPNetmask]; ok {
		out.Netmask = v
	}
	if v, ok := overrides[KeyDHCPGateway]; ok {
		out.Gateway = v
	}
	if v, ok := overrides[KeyDHCPDNS]; ok {
		out.DNS = splitCSV(v)
	}
	if v, ok := overrides[KeyDHCPLeaseHours]; ok {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			out.LeaseHours = n
		}
	}
	if v, ok := overrides[KeyDHCPExclude]; ok {
		out.Exclude = splitCSV(v)
	}
	if out.Mode == "" {
		out.Mode = config.DHCPModeProxy
	}
	return out, nil
}

// ApplyOverridesToConfig overlays any settings rows onto cfg in place. Called
// at controller startup so the boot config picks up the UI's last edits
// without rewriting config.yaml. Returns an error only if the SQL itself
// fails; bad/unparseable values are logged and ignored so a corrupt override
// never bricks the controller.
func ApplyOverridesToConfig(ctx context.Context, store *Store, cfg *config.Config, logger *slog.Logger) error {
	if store == nil || cfg == nil {
		return nil
	}
	overrides, err := store.GetMany(ctx, []string{
		KeyDHCPMode, KeyDHCPStart, KeyDHCPEnd, KeyDHCPNetmask, KeyDHCPGateway,
		KeyDHCPDNS, KeyDHCPLeaseHours, KeyDHCPExclude,
	})
	if err != nil {
		return err
	}
	if len(overrides) == 0 {
		return nil
	}
	if v, ok := overrides[KeyDHCPMode]; ok {
		switch v {
		case config.DHCPModeProxy, config.DHCPModeFull:
			cfg.DHCPMode = v
		default:
			logger.Warn("settings override: unknown dhcp.mode, ignored", "value", v)
		}
	}
	// Pool overrides are only meaningful in full mode.
	if cfg.DHCPMode != config.DHCPModeFull {
		return nil
	}
	if cfg.DHCPPool == nil {
		cfg.DHCPPool = &config.DHCPPool{}
	}
	p := cfg.DHCPPool
	if v, ok := overrides[KeyDHCPStart]; ok {
		p.Start = v
	}
	if v, ok := overrides[KeyDHCPEnd]; ok {
		p.End = v
	}
	if v, ok := overrides[KeyDHCPNetmask]; ok {
		p.Netmask = v
	}
	if v, ok := overrides[KeyDHCPGateway]; ok {
		p.Gateway = v
	}
	if v, ok := overrides[KeyDHCPDNS]; ok {
		p.DNS = splitCSV(v)
	}
	if v, ok := overrides[KeyDHCPLeaseHours]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.LeaseHours = n
		} else {
			logger.Warn("settings override: bad dhcp.pool.lease_hours, ignored", "value", v)
		}
	}
	if v, ok := overrides[KeyDHCPExclude]; ok {
		p.Exclude = splitCSV(v)
	}
	return nil
}

// validateDHCPSettings mirrors config.resolveDHCPPool — same rules, same
// guarantees. Operating on a request struct rather than a *Config so the API
// can reject bad input without mutating any global state.
func validateDHCPSettings(in *DHCPSettings, bootCfg *config.Config) error {
	switch strings.ToLower(strings.TrimSpace(in.Mode)) {
	case config.DHCPModeProxy, config.DHCPModeFull:
		in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	default:
		return fmt.Errorf("mode %q: must be %q or %q", in.Mode, config.DHCPModeProxy, config.DHCPModeFull)
	}
	// In proxy mode pool fields are ignored — clear them so we don't store
	// stale values that would surprise the operator on the next mode flip.
	if in.Mode == config.DHCPModeProxy {
		return nil
	}

	if _, err := parseIPv4Mask(in.Netmask); err != nil {
		return fmt.Errorf("netmask %q: %w", in.Netmask, err)
	}
	gw, err := netip.ParseAddr(in.Gateway)
	if err != nil || !gw.Is4() {
		return fmt.Errorf("gateway %q: must be IPv4", in.Gateway)
	}
	start, err := netip.ParseAddr(in.Start)
	if err != nil || !start.Is4() {
		return fmt.Errorf("start %q: must be IPv4", in.Start)
	}
	end, err := netip.ParseAddr(in.End)
	if err != nil || !end.Is4() {
		return fmt.Errorf("end %q: must be IPv4", in.End)
	}
	if start.Compare(end) > 0 {
		return fmt.Errorf("start %s > end %s", in.Start, in.End)
	}

	// Sanity-check: derive the CIDR from gateway+netmask and require the
	// pool to live inside it. This catches typos like a /24 mask with a
	// pool that spans two networks.
	maskBytes, _ := parseIPv4Mask(in.Netmask)
	ones, _ := maskBytes.Size()
	prefix := netip.PrefixFrom(gw, ones).Masked()
	if !prefix.Contains(start) || !prefix.Contains(end) {
		return fmt.Errorf("pool %s–%s not inside gateway/netmask subnet %s", in.Start, in.End, prefix)
	}

	if len(in.DNS) == 0 {
		in.DNS = []string{"8.8.8.8", "1.1.1.1"}
	} else {
		for _, d := range in.DNS {
			a, err := netip.ParseAddr(d)
			if err != nil || !a.Is4() {
				return fmt.Errorf("dns %q: must be IPv4", d)
			}
		}
	}

	if in.LeaseHours <= 0 {
		in.LeaseHours = 24
	}

	// Always include the controller IP and the gateway in the exclude set
	// — same invariant as config.resolveDHCPPool.
	excludeSet := map[string]struct{}{bootCfg.ServerIP: {}, in.Gateway: {}}
	for _, e := range in.Exclude {
		a, err := netip.ParseAddr(e)
		if err != nil || !a.Is4() {
			return fmt.Errorf("exclude %q: must be IPv4", e)
		}
		excludeSet[a.String()] = struct{}{}
	}
	merged := make([]string, 0, len(excludeSet))
	for ip := range excludeSet {
		merged = append(merged, ip)
	}
	in.Exclude = merged
	return nil
}

func parseIPv4Mask(mask string) (net.IPMask, error) {
	ip := net.ParseIP(mask)
	if ip == nil || ip.To4() == nil {
		return nil, errors.New("not an IPv4 address")
	}
	m := net.IPMask(ip.To4())
	if _, bits := m.Size(); bits != 32 {
		return nil, errors.New("not a contiguous mask")
	}
	return m, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func basicAuthUser(r *http.Request) string {
	if u, _, ok := r.BasicAuth(); ok {
		return u
	}
	return "anonymous"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
