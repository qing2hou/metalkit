package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"metalkit/internal/subnets"
)

// migrateBindingSubnets back-fills bindings.subnet_id from the legacy
// profile.network_json blob. Runs once on every controller startup; idempotent.
//
// Before M2.3-12 the install profile carried gateway/DNS/prefix_len directly,
// and bindings only held the per-host static_address. The subnets table lifts
// the network params out of profile.network so they can be shared. To avoid
// breaking existing rows, the controller startup pass:
//
//   1. Selects every binding row where static_address IS NOT NULL AND
//      subnet_id IS NULL — those need migrating.
//   2. For each, looks up the profile's network_json (method/prefix_len/gateway/dns).
//      Rows with method=dhcp (no network params to migrate) are skipped.
//   3. Computes the network address by masking static_address with prefix_len,
//      finds or creates a subnet row named "auto-<cidr_slug>", and points the
//      binding at it.
//
// Re-runs find the binding already has subnet_id set and skip. The synthetic
// name is deterministic so two bindings sharing a network reuse the same row.
func migrateBindingSubnets(ctx context.Context, db *sql.DB, subnetStore *subnets.Store, logger *slog.Logger) error {
	rows, err := db.QueryContext(ctx, `
        SELECT b.machine_uuid, b.static_address,
               COALESCE(json_extract(p.network_json, '$.method'), '')      AS net_method,
               COALESCE(json_extract(p.network_json, '$.prefix_len'), 0)  AS prefix_len,
               COALESCE(json_extract(p.network_json, '$.gateway'), '')    AS gateway,
               COALESCE(json_extract(p.network_json, '$.dns'), '[]')      AS dns_json
        FROM bindings b
        JOIN profiles p ON p.id = b.profile_id
        WHERE b.static_address IS NOT NULL
          AND b.static_address != ''
          AND b.subnet_id IS NULL`)
	if err != nil {
		return fmt.Errorf("scan bindings for subnet migration: %w", err)
	}
	defer rows.Close()

	type pending struct {
		machineUUID, hostIP, method, gateway, dnsJSON string
		prefixLen                                     int
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.machineUUID, &p.hostIP, &p.method, &p.prefixLen, &p.gateway, &p.dnsJSON); err != nil {
			return fmt.Errorf("scan migration row: %w", err)
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration rows: %w", err)
	}
	if len(todo) == 0 {
		return nil
	}

	migrated := 0
	skipped := 0
	for _, p := range todo {
		// Only static profiles carry the params we need. DHCP bindings stay
		// without a subnet (the install will fall back to profile.network).
		if strings.ToLower(strings.TrimSpace(p.method)) != "static" {
			skipped++
			continue
		}
		if p.prefixLen < 1 || p.prefixLen > 32 || p.gateway == "" {
			logger.Warn("subnet migration: profile lacks usable network params",
				"machine_uuid", p.machineUUID,
				"method", p.method,
				"prefix_len", p.prefixLen,
				"gateway", p.gateway)
			skipped++
			continue
		}
		cidr, err := deriveCIDR(p.hostIP, p.prefixLen)
		if err != nil {
			logger.Warn("subnet migration: cidr derivation failed",
				"machine_uuid", p.machineUUID, "host_ip", p.hostIP, "err", err)
			skipped++
			continue
		}
		var dnsList []string
		if p.dnsJSON != "" && p.dnsJSON != "[]" {
			_ = json.Unmarshal([]byte(p.dnsJSON), &dnsList)
		}
		name := "auto-" + cidrSlug(cidr)
		// Find-or-create by name. Race is unlikely (startup-only) but harmless:
		// duplicate-name on Create just means another path beat us; re-look up.
		existing, err := subnetStore.GetByName(ctx, name)
		var sid string
		switch {
		case err == nil:
			sid = existing.ID
		case errors.Is(err, subnets.ErrNotFound):
			created, err := subnetStore.Create(ctx, subnets.CreateInput{
				Name:        name,
				Description: "auto-migrated from legacy profile network",
				CIDR:        cidr,
				Gateway:     p.gateway,
				DNS:         dnsList,
				CreatedBy:   "system",
			})
			if err != nil {
				logger.Warn("subnet migration: create failed",
					"machine_uuid", p.machineUUID, "cidr", cidr, "err", err)
				skipped++
				continue
			}
			sid = created.ID
		default:
			logger.Warn("subnet migration: lookup failed",
				"machine_uuid", p.machineUUID, "name", name, "err", err)
			skipped++
			continue
		}

		// Direct UPDATE — Upsert() would re-validate everything (machine still
		// exists, image still exists, …) which is unnecessary here. We trust
		// the existing row.
		if _, err := db.ExecContext(ctx,
			`UPDATE bindings SET subnet_id = ? WHERE machine_uuid = ?`, sid, p.machineUUID); err != nil {
			logger.Warn("subnet migration: bind UPDATE failed",
				"machine_uuid", p.machineUUID, "err", err)
			skipped++
			continue
		}
		migrated++
	}
	logger.Info("subnet migration complete",
		"considered", len(todo), "migrated", migrated, "skipped", skipped)
	return nil
}

// deriveCIDR returns the canonical "network/prefix" form for the given host IP
// and prefix length, e.g. ("10.0.0.42", 24) → "10.0.0.0/24".
func deriveCIDR(hostIP string, prefixLen int) (string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(hostIP))
	if err != nil || !addr.Is4() {
		return "", fmt.Errorf("host ip %q: invalid IPv4", hostIP)
	}
	pref, err := addr.Prefix(prefixLen)
	if err != nil {
		return "", fmt.Errorf("prefix %d: %w", prefixLen, err)
	}
	return pref.Masked().String(), nil
}

// cidrSlug turns "192.168.10.0/24" into "192_168_10_0-24" — safe to use as a
// subnet name (matches the name regex [A-Za-z0-9][A-Za-z0-9._-]{0,63}).
func cidrSlug(cidr string) string {
	s := strings.ReplaceAll(cidr, "/", "-")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}
