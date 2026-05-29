package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"metalkit/internal/authapi"
	"metalkit/internal/bindings"
	"metalkit/internal/bmc"
	"metalkit/internal/config"
	"metalkit/internal/crypto"
	"metalkit/internal/dhcp"
	"metalkit/internal/httpd"
	"metalkit/internal/images"
	"metalkit/internal/inventory"
	"metalkit/internal/ipmi"
	"metalkit/internal/ipxebin"
	"metalkit/internal/jobs"
	"metalkit/internal/leases"
	"metalkit/internal/profiles"
	"metalkit/internal/sessions"
	"metalkit/internal/settings"
	"metalkit/internal/sqlitedb"
	"metalkit/internal/subnets"
	"metalkit/internal/tftp"
	"metalkit/internal/util"
	"metalkit/internal/webui"
)

// bmcFetcherAdapter converts bmc.PasswordedCredential into the orchestrator's
// jobs.BMCCredential view so the jobs package stays free of bmc imports.
type bmcFetcherAdapter struct{ store *bmc.Store }

func (a *bmcFetcherAdapter) GetWithPassword(ctx context.Context, machineUUID string) (jobs.BMCCredential, error) {
	pc, err := a.store.GetWithPassword(ctx, machineUUID)
	if err != nil {
		return jobs.BMCCredential{}, err
	}
	return jobs.BMCCredential{
		IP:            pc.IP,
		Port:          pc.Port,
		Username:      pc.Username,
		Password:      pc.Password,
		IPMIInterface: pc.IPMIInterface,
	}, nil
}

// ipmiClientAdapter bridges *ipmi.Client (consumes bmc.PasswordedCredential)
// to the orchestrator's IPMIClient interface (consumes jobs.BMCCredential).
type ipmiClientAdapter struct{ c *ipmi.Client }

func (a *ipmiClientAdapter) BootForPXE(ctx context.Context, cred jobs.BMCCredential) error {
	return a.c.BootForPXE(ctx, toBMCCred(cred))
}

func (a *ipmiClientAdapter) FinalizeBootDisk(ctx context.Context, cred jobs.BMCCredential) error {
	return a.c.FinalizeBootDisk(ctx, toBMCCred(cred))
}

func toBMCCred(c jobs.BMCCredential) bmc.PasswordedCredential {
	return bmc.PasswordedCredential{
		Credential: bmc.Credential{
			IP:            c.IP,
			Port:          c.Port,
			Username:      c.Username,
			IPMIInterface: c.IPMIInterface,
		},
		Password: c.Password,
	}
}

// jobsActiveAdapter satisfies inventory.ActiveJobChecker by treating
// jobs.ErrNotFound as "no active job" rather than propagating it.
type jobsActiveAdapter struct{ store *jobs.Store }

func (a *jobsActiveAdapter) HasActiveJob(ctx context.Context, machineUUID string) (bool, error) {
	_, err := a.store.CurrentForMachine(ctx, machineUUID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, jobs.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// bindingsDeleteAdapter satisfies inventory.BindingDeleter by swallowing the
// "no such binding" error — the machine delete flow tolerates an absent row.
type bindingsDeleteAdapter struct{ store *bindings.Store }

func (a *bindingsDeleteAdapter) DeleteBindingForMachine(ctx context.Context, machineUUID string) error {
	err := a.store.Delete(ctx, machineUUID)
	if err == nil || errors.Is(err, bindings.ErrNotFound) {
		return nil
	}
	return err
}

// leasesAdapter bridges *leases.Store to the dhcp package's LeaseStore
// interface. The DHCP layer wants minimal, protocol-flavored types so it
// stays free of SQL/store imports; we do the struct translation here.
type leasesAdapter struct{ store *leases.Store }

func (a *leasesAdapter) Allocate(ctx context.Context, in dhcp.AllocateInput) (string, error) {
	l, err := a.store.Allocate(ctx, leases.AllocateInput{
		MAC:      in.MAC,
		Hostname: in.Hostname,
		Start:    in.Start,
		End:      in.End,
		Exclude:  in.Exclude,
		LeaseDur: in.LeaseDur,
	})
	if err != nil {
		return "", err
	}
	return l.IP, nil
}

func (a *leasesAdapter) Confirm(ctx context.Context, mac, requestedIP string, leaseDur time.Duration) error {
	_, err := a.store.Confirm(ctx, mac, requestedIP, leaseDur)
	return err
}

func (a *leasesAdapter) Release(ctx context.Context, mac string) error {
	return a.store.Release(ctx, mac)
}

// dhcpReloader implements settings.DHCPReloader. The settings API hands us
// the validated new DHCPSettings; we rebuild a dhcp.Pool and call Reload on
// the live server. The UDP/67 socket stays bound throughout — there's no
// window where DHCP is "down" between save and effective.
type dhcpReloader struct {
	server *dhcp.Server
	leases dhcp.LeaseStore
	logger *slog.Logger
}

func (r *dhcpReloader) ReloadDHCP(_ context.Context, s settings.DHCPSettings) error {
	if s.Mode == config.DHCPModeProxy {
		// Proxy mode: no pool, no leases needed by the protocol layer. The
		// leases store is still alive in the background for the next flip
		// back to full.
		return r.server.Reload(dhcp.ModeProxy, nil, nil)
	}
	pool, err := dhcp.NewPool(
		s.Start, s.End, s.Netmask, s.Gateway,
		s.DNS, uint32(s.LeaseHours)*3600, s.Exclude,
	)
	if err != nil {
		return fmt.Errorf("rebuild pool: %w", err)
	}
	r.logger.Info("dhcp: hot-reloading",
		"mode", s.Mode, "start", s.Start, "end", s.End,
		"gateway", s.Gateway, "netmask", s.Netmask, "lease_hours", s.LeaseHours,
	)
	return r.server.Reload(dhcp.ModeFull, pool, r.leases)
}

// runLeaseGC periodically drops fully-expired lease rows. Loop interval is
// 1 minute (cheap — a DELETE on a small indexed table); grace period is 1h
// so a freshly-released MAC can reconnect and pull back the same IP.
func runLeaseGC(ctx context.Context, store *leases.Store, logger *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := store.GC(ctx, time.Hour)
			if err != nil {
				logger.Warn("leases gc", "err", err)
				continue
			}
			if n > 0 {
				logger.Debug("leases gc", "dropped", n)
			}
		}
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	// Subcommand routing. `metalkit-controller doctor [-config path]` runs the
	// preflight checks and exits; anything else falls through to serve mode.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		fs := flag.NewFlagSet("doctor", flag.ExitOnError)
		cp := fs.String("config", "config.yaml", "path to config file")
		_ = fs.Parse(os.Args[2:])
		return runDoctor(*cp)
	}

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	httpURL := fmt.Sprintf("http://%s%s/boot/ipxe", cfg.ServerIP, cfg.HTTPAddr)

	// Open the shared SQLite handle. The parent directory is created if needed
	// so fresh installs work without a manual mkdir; the same handle is then
	// passed to every store layer (inventory, images, …) so all tables live in
	// one file and cross-table foreign keys are possible.
	//
	// NOTE: SQLite is opened BEFORE the DHCP server because full-mode DHCP
	// needs the leases store, which lives in the same DB. Proxy-mode deploys
	// don't touch the leases table.
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Error("inventory db dir", "path", dir, "err", err)
			return 1
		}
	}
	db, err := sqlitedb.Open(ctx, sqlitedb.Options{
		Path:   cfg.DBPath,
		Logger: logger.With("component", "sqlitedb"),
	})
	if err != nil {
		logger.Error("sqlitedb open", "err", err)
		return 1
	}
	defer db.Close()

	// Settings store: a tiny key/value table the operator UI writes into.
	// Opened early so any UI-saved DHCP overrides overlay cfg BEFORE the
	// DHCP pool / leases store are constructed below — changes saved in
	// the UI then take effect on the next restart without anyone touching
	// config.yaml.
	settingsStore, err := settings.NewStore(ctx, db, logger.With("component", "settings"))
	if err != nil {
		logger.Error("settings open", "err", err)
		return 1
	}
	if err := settings.ApplyOverridesToConfig(ctx, settingsStore, cfg, logger.With("component", "settings")); err != nil {
		logger.Error("settings overlay", "err", err)
		return 1
	}
	// resolveDHCPPool only ran inside config.Load when cfg.DHCPMode was
	// already full at YAML-load time. If the UI just flipped the mode to
	// full via the settings table, re-run the pool resolver now so we get
	// the same auto-derivation that a fresh `dhcpMode: full` install gets.
	if cfg.DHCPMode == config.DHCPModeFull && cfg.DHCPPool == nil {
		if err := cfg.ResolveDHCPPool(); err != nil {
			logger.Error("dhcp pool resolve (post-settings)", "err", err)
			return 1
		}
	}

	// Leases store: always opened, even in proxy mode. The schema apply is
	// cheap (one CREATE TABLE IF NOT EXISTS) and having the store ready
	// from the start means a UI-triggered proxy→full hot-reload doesn't
	// need to lazily wire anything — Reload just hands us the existing
	// store.
	leaseStore, err := leases.NewStore(ctx, db, logger.With("component", "leases"))
	if err != nil {
		logger.Error("leases open", "err", err)
		return 1
	}

	// DHCP pool is only built when full mode is the initial state; if the
	// UI flips mode later, the reloader builds a fresh pool from the new
	// settings without going through this path.
	var dhcpPool *dhcp.Pool
	if cfg.DHCPMode == config.DHCPModeFull {
		dhcpPool, err = dhcp.NewPool(
			cfg.DHCPPool.Start, cfg.DHCPPool.End,
			cfg.DHCPPool.Netmask, cfg.DHCPPool.Gateway,
			cfg.DHCPPool.DNS, uint32(cfg.DHCPPool.LeaseHours)*3600,
			cfg.DHCPPool.Exclude,
		)
		if err != nil {
			logger.Error("dhcp pool", "err", err)
			return 1
		}
		logger.Info("dhcp: full mode enabled",
			"pool_start", cfg.DHCPPool.Start,
			"pool_end", cfg.DHCPPool.End,
			"gateway", cfg.DHCPPool.Gateway,
			"netmask", cfg.DHCPPool.Netmask,
			"lease_hours", cfg.DHCPPool.LeaseHours,
		)
	}

	leasesForDHCP := &leasesAdapter{store: leaseStore}
	dhcpCfg := dhcp.Config{
		Interface:  cfg.Interface,
		ListenAddr: cfg.DHCPAddr,
		ServerIP:   cfg.ServerIP,
		HTTPURL:    httpURL,
		Logger:     logger.With("component", "dhcp"),
		Mode:       dhcp.Mode(cfg.DHCPMode),
		Pool:       dhcpPool,
	}
	if cfg.DHCPMode == config.DHCPModeFull {
		dhcpCfg.Leases = leasesForDHCP
	}
	dhcpSrv, err := dhcp.New(dhcpCfg)
	if err != nil {
		logger.Error("dhcp init", "err", err)
		return 1
	}

	bsdpSrv, err := dhcp.NewBSDP(dhcp.Config{
		Interface:  cfg.Interface,
		ListenAddr: cfg.BSDPAddr,
		ServerIP:   cfg.ServerIP,
		Logger:     logger.With("component", "bsdp"),
	})
	if err != nil {
		logger.Error("bsdp init", "err", err)
		return 1
	}

	tftpFS, err := fs.Sub(ipxebin.FS, "assets")
	if err != nil {
		logger.Error("ipxe assets subfs", "err", err)
		return 1
	}

	tftpSrv, err := tftp.New(tftp.Config{
		ListenAddr: cfg.TFTPAddr,
		Logger:     logger.With("component", "tftp"),
	}, tftpFS)
	if err != nil {
		logger.Error("tftp init", "err", err)
		return 1
	}

	store, err := inventory.NewStore(ctx, db, logger.With("component", "inventory"))
	if err != nil {
		logger.Error("inventory open", "err", err)
		return 1
	}

	// Offline marker: runs until ctx is cancelled.
	go store.RunOfflineMarker(runCtx)

	// Leases GC: drop rows whose expires_at is more than 1h in the past so
	// the leases table doesn't grow unbounded. The 1h grace keeps recently
	// released rows around long enough that a quick MAC reconnect gets the
	// same IP back. Always runs — proxy mode just sees an empty table.
	go runLeaseGC(runCtx, leaseStore, logger.With("component", "leases"))

	// Image catalog + chunked-upload store. Shares the same DB handle so future
	// FKs to images (bindings, jobs) are possible.
	if err := os.MkdirAll(cfg.ImagesDir, 0o755); err != nil {
		logger.Error("images dir", "path", cfg.ImagesDir, "err", err)
		return 1
	}
	imgStore, err := images.NewStore(ctx, db, logger.With("component", "images"), cfg.ImagesDir)
	if err != nil {
		logger.Error("images open", "err", err)
		return 1
	}
	qemu := images.NewQemuImg(logger.With("component", "qemu-img"))
	if !qemu.Available() {
		logger.Warn("qemu-img not on PATH — uploaded images will record fallback metadata only")
	}
	imgAPI := images.NewAPI(imgStore, qemu, logger.With("component", "images-api"))

	// Install profiles catalog. No on-disk side; lives entirely in the shared
	// SQLite DB. Bindings (M2.3-2) will FK back into this table.
	// Hash the cluster-wide default root password once at startup; profiles
	// created with a blank root_password_hash will adopt this hash.
	defaultRootHash, err := util.CryptSHA512(ctx, cfg.DefaultRootPassword)
	if err != nil {
		logger.Error("hash default root password", "err", err)
		return 1
	}
	profileStore, err := profiles.NewStore(ctx, db, logger.With("component", "profiles"), defaultRootHash)
	if err != nil {
		logger.Error("profiles open", "err", err)
		return 1
	}
	profileAPI := profiles.NewAPI(profileStore, logger.With("component", "profiles-api"))

	// Subnet catalog. Operators pre-define network segments (CIDR / gateway /
	// DNS / VLAN) once and pick a subnet per machine at install time.
	subnetStore, err := subnets.NewStore(ctx, db, logger.With("component", "subnets"))
	if err != nil {
		logger.Error("subnets open", "err", err)
		return 1
	}
	subnetAPI := subnets.NewAPI(subnetStore, logger.With("component", "subnets-api"))

	// Per-machine install bindings (machine ↔ image ↔ profile). Schema must
	// apply after machines/images/profiles so its FK declarations resolve.
	// Needs the cipher for the optional per-binding root password column —
	// load the master key first so we can hand it in.
	if dir := filepath.Dir(cfg.MasterKeyPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Error("master key dir", "path", dir, "err", err)
			return 1
		}
	}
	masterKey, err := crypto.LoadOrCreateMaster(cfg.MasterKeyPath)
	if err != nil {
		logger.Error("master key", "path", cfg.MasterKeyPath, "err", err)
		return 1
	}
	cipher, err := crypto.NewCipher(masterKey)
	if err != nil {
		logger.Error("cipher", "err", err)
		return 1
	}

	bindStore, err := bindings.NewStore(ctx, db, logger.With("component", "bindings"), cipher)
	if err != nil {
		logger.Error("bindings open", "err", err)
		return 1
	}
	bindAPI := bindings.NewAPI(bindStore, logger.With("component", "bindings-api"))

	// Refuse subnet DELETE when any binding still points at it (until we
	// promote the FK to ON DELETE RESTRICT proper).
	subnetAPI = subnetAPI.WithBindingRefCount(bindStore.RefCountBySubnet)

	// One-shot back-fill of bindings.subnet_id from legacy profile.network.
	// Idempotent — re-runs find every binding already has subnet_id set and
	// short-circuit. Logged so we can spot rows the heuristic skipped.
	if err := migrateBindingSubnets(ctx, db, subnetStore, logger.With("component", "subnet-migrate")); err != nil {
		logger.Error("subnet migration", "err", err)
		return 1
	}

	bmcStore, err := bmc.NewStore(ctx, db, logger.With("component", "bmc"), cipher)
	if err != nil {
		logger.Error("bmc open", "err", err)
		return 1
	}
	bmcAPI := bmc.NewAPI(bmcStore, logger.With("component", "bmc-api"))

	// Inventory needs the bmc store as a reconciler so a placeholder credential
	// (registered by IP before the host PXE'd) gets migrated to the real SMBIOS
	// UUID on first report.
	store.WithBMCReconciler(bmcStore)

	// Jobs catalog (install/reinstall lifecycle) and the orchestrator that
	// drives binding desired_state → BMC PXE reboot → finalize. ipmitool is
	// optional in lab setups (no BMC) — when missing we skip the IPMI wiring
	// and the orchestrator runs in "agent-pulls-job" mode for affected hosts.
	jobsStore, err := jobs.NewStore(ctx, db, logger.With("component", "jobs"))
	if err != nil {
		logger.Error("jobs open", "err", err)
		return 1
	}
	var ipmiClient *ipmi.Client
	ipmiClient, err = ipmi.NewClient(logger.With("component", "ipmi"), ipmi.ClientOptions{})
	if err != nil {
		logger.Warn("ipmi unavailable — orchestrator will leave jobs pending for the agent to pick up", "err", err)
		ipmiClient = nil
	}
	// Wire the BMC /test endpoint to the ipmi client. *ipmi.Client satisfies
	// bmc.Tester directly (PowerStatus(ctx, bmc.PasswordedCredential)). When
	// ipmiClient is nil, /test returns 503 by design.
	if ipmiClient != nil {
		bmcAPI = bmcAPI.WithTester(ipmiClient)
	}
	orchCfg := jobs.OrchestratorConfig{
		Store:    jobsStore,
		BMC:      &bmcFetcherAdapter{store: bmcStore},
		Bindings: bindStore,
		Logger:   logger.With("component", "orchestrator"),
	}
	if ipmiClient != nil {
		orchCfg.IPMI = &ipmiClientAdapter{c: ipmiClient}
	}
	orch, err := jobs.NewOrchestrator(orchCfg)
	if err != nil {
		logger.Error("orchestrator init", "err", err)
		return 1
	}
	jobsAPI := jobs.NewAPI(jobsStore, logger.With("component", "jobs-api"))
	agentJobsAPI := jobs.NewAgentAPIWithFetchers(jobsStore, bindStore, profileStore, imgStore, logger.With("component", "agent-jobs-api")).
		WithSubnets(subnetStore)

	utilAPI := util.NewAPI(logger.With("component", "util-api"))

	// Browser-UI sessions (cookie auth). Basic Auth keeps working for curl
	// and agents; the cookie path is what makes the form-based login page
	// usable. GC loop runs every hour to evict expired rows.
	sessStore, err := sessions.NewStore(ctx, db, logger.With("component", "sessions"))
	if err != nil {
		logger.Error("sessions open", "err", err)
		return 1
	}
	go sessStore.GCLoop(runCtx, time.Hour)

	authAPI := &authapi.API{
		Sessions:   sessStore,
		AdminUser:  cfg.AdminUser,
		AdminPass:  cfg.AdminPass,
		CookieTTL:  7 * 24 * time.Hour,
		SecureFlag: false, // M2 runs over plain HTTP on the internal net.
		Logger:     logger.With("component", "auth-api"),
	}

	uiHandler := webui.Handler(webui.Config{Mount: "/ui"})

	reloader := &dhcpReloader{
		server: dhcpSrv,
		leases: leasesForDHCP,
		logger: logger.With("component", "dhcp-reload"),
	}
	settingsAPI := settings.NewAPI(settingsStore, cfg, logger.With("component", "settings-api")).
		WithReloader(reloader)

	httpSrv, err := httpd.New(httpd.Config{
		ListenAddr:       cfg.HTTPAddr,
		ServerIP:         cfg.ServerIP,
		BootDir:          cfg.BootDir,
		Logger:           logger.With("component", "httpd"),
		Store:            store,
		MachineActiveJob: &jobsActiveAdapter{store: jobsStore},
		MachineBindings:  &bindingsDeleteAdapter{store: bindStore},
		Images:           imgAPI,
		Profiles:         profileAPI,
		Subnets:          subnetAPI,
		Bindings:         bindAPI,
		BMC:              bmcAPI,
		Jobs:             jobsAPI,
		AgentJobs:        agentJobsAPI,
		Util:             utilAPI,
		Settings:         settingsAPI,
		Sessions:         sessStore,
		Auth:             authAPI,
		UI:               uiHandler,
		AdminUser:        cfg.AdminUser,
		AdminPass:        cfg.AdminPass,
	})
	if err != nil {
		logger.Error("httpd init", "err", err)
		return 1
	}

	type starter struct {
		name  string
		start func(context.Context) error
	}
	servers := []starter{
		{"dhcp", dhcpSrv.Start},
		{"bsdp", bsdpSrv.Start},
		{"tftp", tftpSrv.Start},
		{"httpd", httpSrv.Start},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.start(runCtx)
			if err != nil {
				logger.Error("server exited with error", "server", s.name, "err", err)
				cancel(fmt.Errorf("%s: %w", s.name, err))
				errCh <- err
			}
		}()
	}

	// Orchestrator runs until runCtx is cancelled — it never returns an error
	// (per-tick failures are logged), so it's not in the starter list.
	wg.Add(1)
	go func() {
		defer wg.Done()
		orch.Run(runCtx)
	}()

	<-runCtx.Done()
	logger.Info("shutdown initiated", "cause", context.Cause(runCtx))

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Warn("shutdown timed out after 5s")
	}
	close(errCh)

	for e := range errCh {
		if e != nil && !errors.Is(e, context.Canceled) {
			return 1
		}
	}
	return 0
}
