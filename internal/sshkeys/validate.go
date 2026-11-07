package sshkeys

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxNameLen      = 64
	MaxPublicKeyLen = 8192
	MinBlobLen      = 4 // even the shortest type string ("ssh-dss") overflows this
	MaxKeys         = 32
)

var (
	sshKeyIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// 与 subnets 同款命名规则：字母/数字/文字开头，可含 . _ -，1-64 字符。
	sshKeyNameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._-]{0,63}$`)
)

// knownKeyTypes is the closed set of SSH public key type identifiers we
// accept. Covers everything stock ssh-keygen emits plus the OpenSSH
// security-key (FIDO2) variants; certificate types are deliberately not
// accepted — injecting host CA certs is a different workflow.
var knownKeyTypes = map[string]bool{
	"ssh-ed25519":                     true,
	"ssh-rsa":                         true,
	"ssh-dss":                         true,
	"ecdsa-sha2-nistp256":             true,
	"ecdsa-sha2-nistp384":             true,
	"ecdsa-sha2-nistp521":             true,
	"sk-ssh-ed25519@openssh.com":      true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// ParsedKey is the validated, canonicalised form of one public key line.
type ParsedKey struct {
	// PublicKey is the canonical single-line "type base64 comment" form —
	// whitespace inside the base64 body/comment collapsed to single spaces,
	// safe to write into authorized_keys verbatim.
	PublicKey string
	KeyType   string
	// Fingerprint matches `ssh-keygen -lf`: "SHA256:" + base64(sha256(blob)
	// without padding).
	Fingerprint string
	Comment     string
}

// ParsePublicKey validates a single authorized_keys-format line and returns
// its canonical form plus derived metadata. No third-party SSH library —
// the checks are exactly the ones a hand-rolled parser can do reliably:
//
//   - exactly one line, 2..3 whitespace-separated fields (comment optional)
//   - type in the closed known set
//   - base64 body decodes and the wire-format type string embedded in the
//     blob matches the leading field (catches copy/paste mix-ups that base64
//     alone would accept)
//   - length sane (≤ MaxPublicKeyLen chars, blob ≥ MinBlobLen bytes)
//
// We do NOT verify the key material itself is mathematically valid (curve
// membership etc.) — a key that parses but doesn't validate will simply be
// rejected by sshd at login time, and trusting the stored bytes is no worse
// than the operator pasting them straight into the machine.
func ParsePublicKey(raw string) (ParsedKey, error) {
	if !utf8.ValidString(raw) {
		return ParsedKey{}, fmt.Errorf("public_key: 不是合法 UTF-8")
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return ParsedKey{}, fmt.Errorf("public_key: 不能为空")
	}
	if strings.ContainsAny(s, "\n\r") {
		return ParsedKey{}, fmt.Errorf("public_key: 必须是单行（.pub 文件内容），不要粘贴私钥或多行文本")
	}
	if len(s) > MaxPublicKeyLen {
		return ParsedKey{}, fmt.Errorf("public_key: 长度 %d 超过上限 %d", len(s), MaxPublicKeyLen)
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return ParsedKey{}, fmt.Errorf("public_key: 缺少 base64 密钥体（应为「类型 base64 注释」）")
	}
	if len(fields) > 3 {
		return ParsedKey{}, fmt.Errorf("public_key: 最多 3 个字段（类型 / base64 / 注释），多余空格请去除")
	}
	keyType := fields[0]
	if !knownKeyTypes[keyType] {
		return ParsedKey{}, fmt.Errorf("public_key: 不支持的密钥类型 %q", keyType)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ParsedKey{}, fmt.Errorf("public_key: base64 解码失败: %w", err)
	}
	if len(blob) < MinBlobLen+len("ssh-dss") {
		return ParsedKey{}, fmt.Errorf("public_key: 密钥体过短")
	}
	// The SSH wire format starts with a 4-byte big-endian length + the type
	// string repeated inside the blob. A mismatch means the line was
	// hand-edited or the type prefix was swapped — both produce keys sshd
	// silently ignores, so reject them here with a clear message.
	embedded, ok := wireFormatString(blob)
	if !ok || embedded != keyType {
		return ParsedKey{}, fmt.Errorf("public_key: base64 内容与类型 %q 不匹配", keyType)
	}
	comment := ""
	if len(fields) == 3 {
		comment = fields[2]
	}
	sum := sha256.Sum256(blob)
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	canonical := keyType + " " + fields[1]
	if comment != "" {
		canonical += " " + comment
	}
	return ParsedKey{
		PublicKey:   canonical,
		KeyType:     keyType,
		Fingerprint: fp,
		Comment:     comment,
	}, nil
}

// wireFormatString reads the leading length-prefixed string from an SSH
// wire-format blob. Returns ok=false when the blob is too short to hold one.
func wireFormatString(blob []byte) (string, bool) {
	if len(blob) < 4 {
		return "", false
	}
	n := binary.BigEndian.Uint32(blob[:4])
	if uint64(n) > uint64(len(blob)-4) {
		return "", false
	}
	return string(blob[4 : 4+n]), true
}

// validateName trims and checks the operator-facing key name.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !sshKeyNameRE.MatchString(name) {
		return "", fmt.Errorf("name %q: 1-64 字符，需以字母/数字/文字开头，可含 . _ -", name)
	}
	return name, nil
}

// ValidateIDs canonicalises a binding's ssh_key_ids list: each id must be
// 32 lowercase hex, duplicates removed. Empty ids are dropped. The list
// length is capped so a runaway client can't bloat the binding row.
func ValidateIDs(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for i, raw := range ids {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if !sshKeyIDRE.MatchString(id) {
			return nil, fmt.Errorf("ssh_key_ids[%d] %q: 必须是 32 位小写 hex", i, raw)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) > MaxKeys {
		return nil, fmt.Errorf("ssh_key_ids: 最多 %d 个", MaxKeys)
	}
	return out, nil
}
