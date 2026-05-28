package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ServerIP  string `yaml:"serverIP"`
	Interface string `yaml:"interface"`
	HTTPAddr  string `yaml:"httpAddr"`
	DHCPAddr  string `yaml:"dhcpAddr"`
	BSDPAddr  string `yaml:"bsdpAddr"`
	TFTPAddr  string `yaml:"tftpAddr"`
	BootDir   string `yaml:"bootDir"`
	LogLevel  string `yaml:"logLevel"`

	// Inventory store path. Defaults to /var/lib/metalkit/inventory.db.
	DBPath string `yaml:"dbPath"`

	// Image storage directory. Holds chunked-upload working dirs under .tmp/
	// and the content-addressed final images. Defaults to
	// /var/lib/metalkit/images.
	ImagesDir string `yaml:"imagesDir"`

	// MasterKeyPath holds the AES-256 key used for field-level encryption of
	// BMC passwords (and future sensitive columns). Auto-generated on first
	// boot with mode 0600 if missing. Defaults to /var/lib/metalkit/master.key.
	MasterKeyPath string `yaml:"masterKeyPath"`

	// Basic Auth credentials for the Web UI and the read-side of the inventory
	// API. AdminPass empty disables auth (open mode) and logs a warning at
	// startup. AdminUser defaults to "admin".
	AdminUser string `yaml:"adminUser"`
	AdminPass string `yaml:"adminPass"`

	// DefaultRootPassword is the plaintext password installer baselines all
	// freshly created profiles to when the operator leaves the field blank.
	// Hashed once at controller startup; the hash is what hits SQLite. Default
	// "metalkit" — change in config.yaml per-environment.
	DefaultRootPassword string `yaml:"defaultRootPassword"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	addr, err := netip.ParseAddr(c.ServerIP)
	if err != nil {
		return nil, fmt.Errorf("serverIP %q: %w", c.ServerIP, err)
	}
	if !addr.Is4() {
		return nil, fmt.Errorf("serverIP %q: must be IPv4", c.ServerIP)
	}
	if c.Interface == "" {
		return nil, fmt.Errorf("interface: required")
	}
	if c.BSDPAddr == "" {
		c.BSDPAddr = ":4011"
	}
	if c.DBPath == "" {
		c.DBPath = "/var/lib/metalkit/inventory.db"
	}
	if c.ImagesDir == "" {
		c.ImagesDir = "/var/lib/metalkit/images"
	}
	if c.MasterKeyPath == "" {
		c.MasterKeyPath = "/var/lib/metalkit/master.key"
	}
	if c.AdminUser == "" {
		c.AdminUser = "admin"
	}
	if c.DefaultRootPassword == "" {
		c.DefaultRootPassword = "metalkit"
	}
	return &c, nil
}

func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
