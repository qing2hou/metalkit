package jobs

import (
	"fmt"
	"regexp"
	"strings"
)

var smbiosUUIDRE = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var hex32RE = regexp.MustCompile(`^[0-9a-f]{32}$`)

var validJobTypes = map[string]bool{
	"install": true, "reinstall": true,
}

var validJobStatuses = map[string]bool{
	"pending":   true,
	"running":   true,
	"succeeded": true,
	"failed":    true,
	"cancelled": true,
}

var validLogLevels = map[string]bool{
	"debug": true, "info": true, "warn": true, "error": true,
}

// MaxStageLen / MaxErrorLen / MaxLogMessageLen bound DB row size and keep
// rogue agents from filling the disk via job_logs.
const (
	MaxStageLen      = 64
	MaxErrorLen      = 4096
	MaxLogMessageLen = 4096
)

func validateMachineUUID(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if !smbiosUUIDRE.MatchString(u) {
		return "", fmt.Errorf("machine_uuid %q: must be canonical lowercase SMBIOS UUID", u)
	}
	return u, nil
}

func validateID(field, id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !hex32RE.MatchString(id) {
		return "", fmt.Errorf("%s %q: must be 32 lowercase hex chars", field, id)
	}
	return id, nil
}

func validateJobID(id string) (string, error) {
	return validateID("job_id", id)
}

func validateType(t string) (string, error) {
	t = strings.ToLower(strings.TrimSpace(t))
	if !validJobTypes[t] {
		return "", fmt.Errorf("type %q: must be install or reinstall", t)
	}
	return t, nil
}

func validateStage(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > MaxStageLen {
		return "", fmt.Errorf("stage length %d exceeds %d", len(s), MaxStageLen)
	}
	return s, nil
}

func validateError(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > MaxErrorLen {
		return s[:MaxErrorLen], nil // truncate, not fail — we always want the row written
	}
	return s, nil
}

func validateLogLevel(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !validLogLevels[s] {
		return "", fmt.Errorf("level %q: must be debug/info/warn/error", s)
	}
	return s, nil
}

func validateLogMessage(s string) (string, error) {
	if len(s) > MaxLogMessageLen {
		s = s[:MaxLogMessageLen]
	}
	return s, nil
}
