package main

// `metalkit doctor` is the preflight / health-check subcommand. It catches
// the common deploy-time mistakes that otherwise surface as cryptic
// failures hours later: missing boot artifacts, ports already in use,
// serverIP not bound on the configured interface, missing external tools
// (ipmitool / mkpasswd / qemu-img), or a too-permissive master.key.
//
// Output format is line-per-check `[PASS|WARN|FAIL] description (detail)`;
// returns exit code 0 if everything is PASS/WARN, 1 if any FAIL.
//
// Invoke either before starting the controller (the install script runs it
// at the end) or in production via systemd ExecStartPre when desired.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"metalkit/internal/config"
)

type checkResult struct {
	name   string
	status string // PASS / WARN / FAIL
	detail string
}

func runDoctor(configPath string) int {
	var results []checkResult

	cfg, err := config.Load(configPath)
	if err != nil {
		results = append(results, checkResult{"config load", "FAIL", err.Error()})
		printResults(results)
		return 1
	}
	results = append(results, checkResult{"config load", "PASS", configPath})

	results = append(results, checkInterface(cfg))
	results = append(results, checkPorts(cfg)...)
	results = append(results, checkBootDir(cfg))
	results = append(results, checkDataDirs(cfg)...)
	results = append(results, checkMasterKey(cfg))
	results = append(results, checkAdminAuth(cfg))
	results = append(results, checkExternalTools()...)

	printResults(results)
	failed := false
	bindFail := false
	for _, r := range results {
		if r.status == "FAIL" {
			failed = true
			if strings.Contains(r.detail, "address already in use") {
				bindFail = true
			}
		}
	}
	if bindFail {
		fmt.Fprintln(os.Stderr,
			"\nnote: 'address already in use' usually means metalkit-controller is "+
				"already running on this host — stop it (systemctl stop metalkit-controller) "+
				"and re-run doctor for a clean preflight.")
	}
	if failed {
		return 1
	}
	return 0
}

func printResults(results []checkResult) {
	for _, r := range results {
		marker := r.status
		switch r.status {
		case "PASS":
			marker = "[ \033[32mPASS\033[0m ]"
		case "WARN":
			marker = "[ \033[33mWARN\033[0m ]"
		case "FAIL":
			marker = "[ \033[31mFAIL\033[0m ]"
		}
		line := fmt.Sprintf("%s  %s", marker, r.name)
		if r.detail != "" {
			line += "  — " + r.detail
		}
		fmt.Println(line)
	}
}

// checkInterface ensures cfg.Interface exists and cfg.ServerIP is one of
// its IPv4 addresses. A common deploy mistake is listing the wrong NIC
// name in config.yaml after moving the controller to a new host.
func checkInterface(cfg *config.Config) checkResult {
	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return checkResult{"network interface", "FAIL",
			fmt.Sprintf("interface %q not found: %v", cfg.Interface, err)}
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return checkResult{"network interface", "FAIL",
			fmt.Sprintf("read addrs on %q: %v", cfg.Interface, err)}
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.To4() != nil && ipnet.IP.String() == cfg.ServerIP {
			return checkResult{"network interface", "PASS",
				fmt.Sprintf("%s has %s", cfg.Interface, cfg.ServerIP)}
		}
	}
	return checkResult{"network interface", "FAIL",
		fmt.Sprintf("serverIP %s not bound on %s", cfg.ServerIP, cfg.Interface)}
}

// checkPorts probes each UDP/TCP port by attempting a non-listening bind.
// Reports WARN when "address already in use" if it's port 67 (the dnsmasq
// sidecar coexistence pattern is intentional) and FAIL otherwise.
func checkPorts(cfg *config.Config) []checkResult {
	out := []checkResult{
		checkUDPPort("DHCP/BSDP port 67", cfg.DHCPAddr, true),
		checkUDPPort("BSDP port 4011", cfg.BSDPAddr, false),
		checkUDPPort("TFTP port 69", cfg.TFTPAddr, false),
		checkTCPPort("HTTP "+cfg.HTTPAddr, cfg.HTTPAddr),
	}
	return out
}

func checkUDPPort(name, addr string, allowCoexist bool) checkResult {
	if addr == "" {
		return checkResult{name, "WARN", "addr empty in config"}
	}
	c, err := net.ListenPacket("udp", addr)
	if err == nil {
		_ = c.Close()
		return checkResult{name, "PASS", "bindable on " + addr}
	}
	if allowCoexist && isAddrInUse(err) {
		return checkResult{name, "WARN",
			"in use — OK if dnsmasq sidecar is intentional (SO_REUSEADDR coexistence)"}
	}
	return checkResult{name, "FAIL", err.Error()}
}

func checkTCPPort(name, addr string) checkResult {
	if addr == "" {
		return checkResult{name, "WARN", "addr empty in config"}
	}
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
		return checkResult{name, "PASS", "bindable on " + addr}
	}
	return checkResult{name, "FAIL", err.Error()}
}

func isAddrInUse(err error) bool {
	var se syscall.Errno
	if errors.As(err, &se) {
		return se == syscall.EADDRINUSE
	}
	return strings.Contains(err.Error(), "address already in use")
}

// checkBootDir ensures BootDir is a directory containing the three artifacts
// that PXE boot requires. Missing them produces TFTP 404 / iPXE timeout
// failures that are obvious in retrospect but baffling at first sight.
func checkBootDir(cfg *config.Config) checkResult {
	fi, err := os.Stat(cfg.BootDir)
	if err != nil {
		return checkResult{"boot artifacts", "FAIL",
			fmt.Sprintf("bootDir %s: %v", cfg.BootDir, err)}
	}
	if !fi.IsDir() {
		return checkResult{"boot artifacts", "FAIL", cfg.BootDir + " is not a directory"}
	}
	required := []string{"vmlinuz", "initrd.img", "filesystem.squashfs"}
	missing := []string{}
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(cfg.BootDir, name)); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return checkResult{"boot artifacts", "FAIL",
			"missing in " + cfg.BootDir + ": " + strings.Join(missing, ", ") +
				" — run `make live` or copy from a previous deploy"}
	}
	return checkResult{"boot artifacts", "PASS",
		fmt.Sprintf("%s has vmlinuz, initrd.img, filesystem.squashfs", cfg.BootDir)}
}

// checkDataDirs verifies DBPath / ImagesDir parents are writable so we
// can create them at startup if absent.
func checkDataDirs(cfg *config.Config) []checkResult {
	out := []checkResult{}
	for _, c := range []struct {
		name string
		path string
		isFile bool
	}{
		{"db path", cfg.DBPath, true},
		{"images dir", cfg.ImagesDir, false},
	} {
		target := c.path
		if c.isFile {
			target = filepath.Dir(c.path)
		}
		if err := ensureWritableDir(target); err != nil {
			out = append(out, checkResult{c.name, "FAIL",
				fmt.Sprintf("%s: %v", c.path, err)})
			continue
		}
		out = append(out, checkResult{c.name, "PASS", c.path + " writable"})
	}
	return out
}

func ensureWritableDir(path string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		// Try to create it as proof of writability.
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	// Write probe.
	probe := filepath.Join(path, ".metalkit-doctor-probe")
	f, err := os.Create(probe)
	if err != nil {
		return err
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// checkMasterKey: if file exists, must be mode 0600 (otherwise FAIL — a
// world-readable key defeats the encryption). If absent, parent dir must
// be writable so first-boot can generate it.
func checkMasterKey(cfg *config.Config) checkResult {
	fi, err := os.Stat(cfg.MasterKeyPath)
	if errors.Is(err, os.ErrNotExist) {
		if e := ensureWritableDir(filepath.Dir(cfg.MasterKeyPath)); e != nil {
			return checkResult{"master key", "FAIL",
				fmt.Sprintf("parent dir not writable: %v", e)}
		}
		return checkResult{"master key", "WARN",
			"will be generated on first start"}
	}
	if err != nil {
		return checkResult{"master key", "FAIL", err.Error()}
	}
	mode := fi.Mode().Perm()
	if mode != 0o600 {
		return checkResult{"master key", "FAIL",
			fmt.Sprintf("%s mode is %o, want 0600 — `chmod 0600 %s`",
				cfg.MasterKeyPath, mode, cfg.MasterKeyPath)}
	}
	return checkResult{"master key", "PASS",
		cfg.MasterKeyPath + " mode 0600"}
}

// checkAdminAuth warns when adminPass is empty (open mode). The controller
// still starts and prints a warning at runtime, but the doctor reports it
// explicitly so install scripts can surface it.
func checkAdminAuth(cfg *config.Config) checkResult {
	if cfg.AdminPass == "" {
		return checkResult{"admin auth", "WARN",
			"adminPass empty — UI and read API run unauthenticated"}
	}
	return checkResult{"admin auth", "PASS",
		"user=" + cfg.AdminUser + ", password set"}
}

// checkExternalTools is non-fatal: ipmitool / mkpasswd / qemu-img are
// invoked at runtime by orchestrator / util / image-lint paths. Missing
// any one of them disables a feature but doesn't kill the controller.
func checkExternalTools() []checkResult {
	tools := []struct {
		name     string
		bin      string
		feature  string
	}{
		{"ipmitool", "ipmitool", "BMC power / boot-device control"},
		{"mkpasswd", "mkpasswd", "POST /api/v1/util/crypt-sha512 (UI password helper)"},
		{"qemu-img", "qemu-img", "image lint / format detection on upload"},
	}
	out := []checkResult{}
	for _, t := range tools {
		if _, err := exec.LookPath(t.bin); err != nil {
			out = append(out, checkResult{
				"tool: " + t.name, "WARN",
				"not in PATH — " + t.feature + " unavailable",
			})
			continue
		}
		out = append(out, checkResult{
			"tool: " + t.name, "PASS",
			"available (" + t.feature + ")",
		})
	}
	return out
}
