package images

import (
	"regexp"
	"strings"
)

// DetectionResult captures what we could deduce about an uploaded image from
// its filename. It's a best-effort hint: when a field is empty we mean "could
// not infer", and the operator's explicit value (from the upload form) wins.
//
// We deliberately keep this filename-only — no libguestfs / no qemu-nbd / no
// mount — so the image upload path stays dependency-free. Deeper introspection
// (UEFI/BIOS partition tables, cloud-init service detection) can be layered
// on later if operators ask for it; the data model is already in place.
type DetectionResult struct {
	Family  string // "ubuntu" / "debian" / "rhel" / "rhel7" / "kylin" / "openeuler" / "opensuse" / "" (unknown)
	Version string // distro version, e.g. "22.04", "12", "9", "7"
}

// reKnown maps a substring/regex against canonical filenames published by
// upstream cloud-image vendors. First match wins — order matters: more
// specific families before broader ones (rocky/alma before rhel; centos7
// before centos).
var detectionRules = []struct {
	name    string
	pattern *regexp.Regexp
	family  string
	// version capture: nil means "use versionFromName(name) generic fallback"
	versionRE *regexp.Regexp
}{
	// Ubuntu cloud images: jammy-server-cloudimg-amd64.img, focal-...,
	// ubuntu-22.04-server-cloudimg-amd64.qcow2, etc.
	{
		name:      "ubuntu-codename",
		pattern:   regexp.MustCompile(`(?i)(^|[-_./])(noble|jammy|focal|bionic|xenial|trusty)([-_./]|$)`),
		family:    "ubuntu",
		versionRE: regexp.MustCompile(`(?i)(noble|jammy|focal|bionic|xenial|trusty)`),
	},
	{
		name:      "ubuntu-numeric",
		pattern:   regexp.MustCompile(`(?i)ubuntu[-_.]?(\d+\.\d+)`),
		family:    "ubuntu",
		versionRE: regexp.MustCompile(`(?i)ubuntu[-_.]?(\d+\.\d+)`),
	},
	// Debian cloud images: debian-12-genericcloud-amd64.qcow2,
	// debian-11.7.0-amd64-netinst.iso, debian-bookworm-...
	{
		name:      "debian-numeric",
		pattern:   regexp.MustCompile(`(?i)debian[-_.]?(\d+)`),
		family:    "debian",
		versionRE: regexp.MustCompile(`(?i)debian[-_.]?(\d+)`),
	},
	{
		name:    "debian-codename",
		pattern: regexp.MustCompile(`(?i)(^|[-_./])(bookworm|bullseye|buster|stretch|trixie)([-_./]|$)`),
		family:  "debian",
	},
	// CentOS 7 — special-case before rhel because installer/seed.go treats
	// rhel7 differently (sysconfig ifcfg path instead of NetworkManager keyfile).
	{
		name:      "centos7",
		pattern:   regexp.MustCompile(`(?i)centos[-_.]?7([-_.]|$)`),
		family:    "rhel7",
		versionRE: regexp.MustCompile(`(?i)centos[-_.]?(7(?:\.\d+)?)`),
	},
	// Rocky / AlmaLinux / RHEL / Oracle / Fedora — all collapse to "rhel" family
	// in our model since they share the NetworkManager + dracut + selinux pipeline.
	// Match version digit (8, 9, …) for downstream profile matching.
	{
		name:      "rocky",
		pattern:   regexp.MustCompile(`(?i)rocky[-_.]?(\d+)`),
		family:    "rhel",
		versionRE: regexp.MustCompile(`(?i)rocky[-_.]?(\d+(?:\.\d+)?)`),
	},
	{
		name:      "almalinux",
		pattern:   regexp.MustCompile(`(?i)almalinux[-_.]?(\d+)`),
		family:    "rhel",
		versionRE: regexp.MustCompile(`(?i)almalinux[-_.]?(\d+(?:\.\d+)?)`),
	},
	{
		name:      "centos",
		pattern:   regexp.MustCompile(`(?i)centos[-_.]?(\d+)`),
		family:    "rhel",
		versionRE: regexp.MustCompile(`(?i)centos[-_.]?(\d+(?:\.\d+)?)`),
	},
	{
		name:      "rhel",
		pattern:   regexp.MustCompile(`(?i)rhel[-_.]?(\d+)`),
		family:    "rhel",
		versionRE: regexp.MustCompile(`(?i)rhel[-_.]?(\d+(?:\.\d+)?)`),
	},
	{
		name:      "oracle",
		pattern:   regexp.MustCompile(`(?i)(ol|oracle)[-_.]?(\d+)`),
		family:    "rhel",
		versionRE: regexp.MustCompile(`(?i)(?:ol|oracle)[-_.]?(\d+(?:\.\d+)?)`),
	},
	{
		// Fedora cloud filenames are `Fedora-Cloud-Base-40-...` with words
		// between "fedora" and the version — accept any non-digit run.
		name:      "fedora",
		pattern:   regexp.MustCompile(`(?i)fedora`),
		family:    "rhel",
		versionRE: regexp.MustCompile(`(?i)fedora[^0-9]+(\d{2,3})\b`),
	},
	// 银河麒麟 Kylin — V10 基于 Ubuntu，V4 基于 CentOS
	{
		name:      "kylin",
		pattern:   regexp.MustCompile(`(?i)kylin[-_.]?v?(\d+)`),
		family:    "kylin",
		versionRE: regexp.MustCompile(`(?i)kylin[-_.]?v?(\d+(?:\.\d+)?)`),
	},
	// openEuler — 基于 CentOS/RHEL 架构
	{
		name:      "openeuler",
		pattern:   regexp.MustCompile(`(?i)openeuler[-_.]?(\d+)`),
		family:    "openeuler",
		versionRE: regexp.MustCompile(`(?i)openeuler[-_.]?(\d+(?:\.\d+)?)`),
	},
	// openSUSE Leap — uses Wicked or NetworkManager
	{
		name:      "opensuse-leap",
		pattern:   regexp.MustCompile(`(?i)opensuse[-_.]?leap[-_.]?(\d+)`),
		family:    "opensuse",
		versionRE: regexp.MustCompile(`(?i)opensuse[-_.]?leap[-_.]?(\d+(?:\.\d+)?)`),
	},
	// openSUSE Tumbleweed (rolling, no version number)
	{
		name:    "opensuse-tumbleweed",
		pattern: regexp.MustCompile(`(?i)opensuse[-_.]tumbleweed`),
		family:  "opensuse",
	},
}

// ubuntuCodenameToVersion maps Ubuntu codenames to numeric LTS releases. Only
// the LTS series and a handful of recent non-LTS releases that show up in
// production fleets.
var ubuntuCodenameToVersion = map[string]string{
	"noble":   "24.04",
	"jammy":   "22.04",
	"focal":   "20.04",
	"bionic":  "18.04",
	"xenial":  "16.04",
	"trusty":  "14.04",
}

// debianCodenameToVersion translates common Debian codenames to numeric.
var debianCodenameToVersion = map[string]string{
	"trixie":   "13",
	"bookworm": "12",
	"bullseye": "11",
	"buster":   "10",
	"stretch":  "9",
}

// DetectFromFilename inspects a filename (just the name, no path) and returns
// what we can infer about family + version. An empty result means we could
// not match any pattern, which is fine — the operator can fill it in.
func DetectFromFilename(name string) DetectionResult {
	if name == "" {
		return DetectionResult{}
	}
	lower := strings.ToLower(name)

	for _, rule := range detectionRules {
		if !rule.pattern.MatchString(lower) {
			continue
		}
		res := DetectionResult{Family: rule.family}
		if rule.versionRE != nil {
			if m := rule.versionRE.FindStringSubmatch(lower); m != nil {
				// Last sub-match: version capture is the final group.
				token := m[len(m)-1]
				if v, ok := ubuntuCodenameToVersion[token]; ok {
					res.Version = v
				} else if v, ok := debianCodenameToVersion[token]; ok {
					res.Version = v
				} else {
					res.Version = token
				}
			}
		} else {
			// Codename-only rule: look up directly.
			if m := rule.pattern.FindStringSubmatch(lower); m != nil {
				for _, tok := range m {
					if v, ok := debianCodenameToVersion[tok]; ok {
						res.Version = v
						break
					}
					if v, ok := ubuntuCodenameToVersion[tok]; ok {
						res.Version = v
						break
					}
				}
			}
		}
		return res
	}

	return DetectionResult{}
}

// MergeDetected returns a copy of in with empty Family/Version filled from det.
// Operator-supplied values always win; this is a best-effort backfill.
func MergeDetected(in CreateUploadInput, det DetectionResult) CreateUploadInput {
	out := in
	if out.Family == "" && det.Family != "" {
		out.Family = det.Family
	}
	if out.Version == "" && det.Version != "" {
		out.Version = det.Version
	}
	return out
}
