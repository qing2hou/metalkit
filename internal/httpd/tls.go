package httpd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"log/slog"
)

// HTTPSConfig selects the TLS mode for the HTTP listener (mirrors the
// config.yaml `https:` block; defined here so httpd doesn't import config).
//
//   - static: CertFile + KeyFile point at operator-provided PEM files
//   - auto:   both empty → self-signed certificate generated on first boot
//     into StateDir, SANs cover ServerIP + localhost + all interface IPs
//     (plus AutoDNSName when set).
type HTTPSConfig struct {
	// HTTPSAddr is the TLS listener address (e.g. ":8443"). The config
	// loader fills the default; see config.HTTPSConfig for the dual-
	// listener model.
	HTTPSAddr   string
	CertFile    string
	KeyFile     string
	StateDir    string
	AutoDNSName string
}

// ensureTLSMaterial returns a usable tls.Certificate for the configured
// HTTPS mode:
//
//   - static mode (certFile+keyFile): loaded from disk on every start so a
//     rotated certificate is picked up by a mere restart;
//   - auto mode (no files): a self-signed ECDSA P-256 certificate is
//     generated on first boot, stored PEM-encoded under stateDir (0640 for
//     the key) and reused afterwards. It carries SANs for cfg IPs and
//     hostnames and a 10-year validity — long enough that rotate-on-renew
//     is a non-issue for management networks.
func ensureTLSMaterial(cfg HTTPSConfig, serverIP, autoDNSName string, logger *slog.Logger) (tls.Certificate, error) {
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("https: load cert/key: %w", err)
		}
		return cert, nil
	}

	dir := cfg.StateDir
	if dir == "" {
		return tls.Certificate{}, errors.New("https: stateDir empty")
	}
	certPath := filepath.Join(dir, "auto-cert.pem")
	keyPath := filepath.Join(dir, "auto-key.pem")

	// Reuse an existing auto pair if present and still valid.
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err == nil && time.Now().Before(leaf.NotAfter.Add(-30*24*time.Hour)) {
			logger.Info("https: reusing auto-generated certificate", "cert", certPath, "not_after", leaf.NotAfter.Format(time.RFC3339))
			return cert, nil
		}
		logger.Warn("https: auto certificate missing/expiring soon — regenerating", "cert", certPath)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("https: mkdir %s: %w", dir, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("https: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("https: serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "metalkit-controller",
			Organization: []string{"metalkit"},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// SANs: every name/IP the operators might type into a browser.
	sans := []string{"localhost"}
	if autoDNSName != "" {
		sans = append(sans, autoDNSName)
	}
	for _, ip := range []string{serverIP, "127.0.0.1"} {
		if ip != "" {
			sans = append(sans, ip)
		}
	}
	// Interface IPs beyond serverIP (e.g. a secondary mgmt address) are
	// covered by including all local addresses once.
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
				s := ipnet.IP.String()
				dup := false
				for _, v := range sans {
					if v == s {
						dup = true
						break
					}
				}
				if !dup {
					sans = append(sans, s)
				}
			}
		}
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("https: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	derKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("https: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: derKey})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("https: write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("https: write key: %w", err)
	}
	logger.Info("https: generated self-signed certificate",
		"cert", certPath, "sans", sans, "not_after", tmpl.NotAfter.Format(time.RFC3339))

	return tls.X509KeyPair(certPEM, keyPEM)
}
