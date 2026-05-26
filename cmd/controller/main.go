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
	"metalkit/internal/profiles"
	"metalkit/internal/sessions"
	"metalkit/internal/sqlitedb"
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

func main() {
	os.Exit(run())
}

func run() int {
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

	dhcpSrv, err := dhcp.New(dhcp.Config{
		Interface:  cfg.Interface,
		ListenAddr: cfg.DHCPAddr,
		ServerIP:   cfg.ServerIP,
		HTTPURL:    httpURL,
		Logger:     logger.With("component", "dhcp"),
	})
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

	// Open the shared SQLite handle. The parent directory is created if needed
	// so fresh installs work without a manual mkdir; the same handle is then
	// passed to every store layer (inventory, images, …) so all tables live in
	// one file and cross-table foreign keys are possible.
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

	store, err := inventory.NewStore(ctx, db, logger.With("component", "inventory"))
	if err != nil {
		logger.Error("inventory open", "err", err)
		return 1
	}

	// Offline marker: runs until ctx is cancelled.
	go store.RunOfflineMarker(runCtx)

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
	profileStore, err := profiles.NewStore(ctx, db, logger.With("component", "profiles"))
	if err != nil {
		logger.Error("profiles open", "err", err)
		return 1
	}
	profileAPI := profiles.NewAPI(profileStore, logger.With("component", "profiles-api"))

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
	agentJobsAPI := jobs.NewAgentAPIWithFetchers(jobsStore, bindStore, profileStore, imgStore, logger.With("component", "agent-jobs-api"))

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
		Bindings:         bindAPI,
		BMC:              bmcAPI,
		Jobs:             jobsAPI,
		AgentJobs:        agentJobsAPI,
		Util:             utilAPI,
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
