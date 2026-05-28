#!/usr/bin/env bash
# scripts/install.sh — one-command native deploy of metalkit controller.
#
# Goals: works on Debian/Ubuntu + RHEL family (CentOS / Rocky / Alma) with
# no Docker. Installs runtime dependencies via the system package manager,
# drops binaries + boot artifacts under /opt/metalkit, generates a config,
# installs a systemd unit, and runs `metalkit-controller doctor` at the end
# so any deploy mistake surfaces immediately.
#
# Idempotent: re-running upgrades binaries / artifacts in place and leaves
# /etc/metalkit/config.yaml + /var/lib/metalkit data untouched.
#
# Layout after install:
#   /opt/metalkit/bin/{controller,agent}        — binaries (root:root 0755)
#   /opt/metalkit/boot/{vmlinuz,initrd.img,filesystem.squashfs}
#   /etc/metalkit/config.yaml                    — config (root:root 0640)
#   /var/lib/metalkit/{inventory.db,images,master.key}
#   /etc/systemd/system/metalkit-controller.service
#
# Usage (interactive):
#   sudo ./scripts/install.sh
#
# Usage (non-interactive, e.g. in CI / fleet rollout):
#   sudo MK_INTERFACE=eno1 MK_SERVER_IP=10.0.0.5 MK_ADMIN_PASS='s3cret' \
#        ./scripts/install.sh --yes

set -euo pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
REPO_DIR="$( cd -- "${SCRIPT_DIR}/.." &> /dev/null && pwd )"

# Tunables (overridable via env or flags)
PREFIX="${MK_PREFIX:-/opt/metalkit}"
CONFIG_DIR="${MK_CONFIG_DIR:-/etc/metalkit}"
DATA_DIR="${MK_DATA_DIR:-/var/lib/metalkit}"
SERVICE_NAME="metalkit-controller"
NONINTERACTIVE=0
SKIP_DEPS=0
SKIP_DOCTOR=0

usage() {
    cat <<EOF
Usage: $(basename "$0") [options]

Native installer for metalkit-controller. Runs on Debian/Ubuntu and RHEL
family (CentOS / Rocky / Alma). Does NOT use Docker.

Options:
  -y, --yes            Non-interactive. Read MK_* env vars instead of prompting.
      --skip-deps      Don't apt/dnf install runtime packages.
      --skip-doctor    Don't run 'controller doctor' at the end.
  -h, --help           Show this help.

Environment (non-interactive mode):
  MK_INTERFACE         Network interface metalkit binds (e.g. eno1)
  MK_SERVER_IP         IPv4 address bound on that interface
  MK_ADMIN_USER        Admin username for UI / API (default: admin)
  MK_ADMIN_PASS        Admin password (required; empty disables auth and is unsafe)
  MK_HTTP_ADDR         Listen addr for HTTP (default: :8080)

Repo layout expected:
  ${REPO_DIR}/bin/{controller,agent}
  ${REPO_DIR}/boot/{vmlinuz,initrd.img,filesystem.squashfs}
EOF
}

log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 0. Args + preflight
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        -y|--yes)       NONINTERACTIVE=1; shift ;;
        --skip-deps)    SKIP_DEPS=1; shift ;;
        --skip-doctor)  SKIP_DOCTOR=1; shift ;;
        -h|--help)      usage; exit 0 ;;
        *) die "unknown option: $1 (try --help)" ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    die "must run as root (sudo $0)"
fi

# Confirm the artifact layout.
for f in "${REPO_DIR}/bin/controller" "${REPO_DIR}/bin/agent"; do
    [[ -x "$f" ]] || die "missing binary: $f — run 'go build ./cmd/controller && go build ./cmd/agent' first"
done
for f in vmlinuz initrd.img filesystem.squashfs; do
    [[ -f "${REPO_DIR}/boot/$f" ]] || die "missing boot artifact: ${REPO_DIR}/boot/$f — run scripts/build-live.sh first"
done

# ---------------------------------------------------------------------------
# 1. Detect OS family + install runtime dependencies
# ---------------------------------------------------------------------------

OS_FAMILY=unknown
if [[ -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    case "${ID:-}" in
        debian|ubuntu)                            OS_FAMILY=debian ;;
        rhel|centos|rocky|almalinux|fedora|ol)    OS_FAMILY=rhel ;;
    esac
    if [[ "$OS_FAMILY" == unknown ]]; then
        for like in ${ID_LIKE:-}; do
            case "$like" in
                debian|ubuntu) OS_FAMILY=debian; break ;;
                rhel|fedora)   OS_FAMILY=rhel; break ;;
            esac
        done
    fi
fi
[[ "$OS_FAMILY" == unknown ]] && die "unsupported distro — only debian/ubuntu and rhel family are supported"
log "detected OS family: $OS_FAMILY (${PRETTY_NAME:-unknown})"

install_deps() {
    if [[ "$SKIP_DEPS" == 1 ]]; then
        log "skipping dependency install (--skip-deps)"
        return
    fi
    log "installing runtime dependencies via $OS_FAMILY package manager"
    case "$OS_FAMILY" in
        debian)
            # qemu-utils → qemu-img; whois → mkpasswd; ipmitool → BMC control;
            # cloud-image-utils → cloud-localds (seed ISO). xorriso optional fallback.
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq
            apt-get install -y --no-install-recommends \
                ipmitool whois qemu-utils cloud-image-utils ca-certificates curl
            ;;
        rhel)
            # whois on RHEL 9 carries mkpasswd; on RHEL 7/8 it lives in expect; try both.
            local pkgmgr=dnf
            command -v dnf >/dev/null 2>&1 || pkgmgr=yum
            "$pkgmgr" install -y epel-release || true
            "$pkgmgr" install -y ipmitool qemu-img genisoimage ca-certificates curl
            # mkpasswd may come from whois (most distros) or be missing on RHEL 7 minimal;
            # if absent we'll warn at doctor time.
            "$pkgmgr" install -y whois || true
            ;;
    esac
}

install_deps

# ---------------------------------------------------------------------------
# 2. Lay down directories + binaries + boot artifacts
# ---------------------------------------------------------------------------

log "creating ${PREFIX}, ${CONFIG_DIR}, ${DATA_DIR}"
install -d -m 0755 "${PREFIX}/bin" "${PREFIX}/boot"
install -d -m 0755 "${CONFIG_DIR}"
install -d -m 0750 "${DATA_DIR}" "${DATA_DIR}/images"

log "installing binaries"
install -m 0755 "${REPO_DIR}/bin/controller" "${PREFIX}/bin/metalkit-controller"
install -m 0755 "${REPO_DIR}/bin/agent"      "${PREFIX}/bin/metalkit-agent"

log "installing boot artifacts to ${PREFIX}/boot"
install -m 0644 "${REPO_DIR}/boot/vmlinuz"            "${PREFIX}/boot/"
install -m 0644 "${REPO_DIR}/boot/initrd.img"         "${PREFIX}/boot/"
install -m 0644 "${REPO_DIR}/boot/filesystem.squashfs" "${PREFIX}/boot/"

# ---------------------------------------------------------------------------
# 3. Generate /etc/metalkit/config.yaml (preserve existing on upgrade)
# ---------------------------------------------------------------------------

CONFIG_FILE="${CONFIG_DIR}/config.yaml"

prompt() {
    local var="$1" prompt_text="$2" default="${3:-}" silent="${4:-}"
    local val=""
    if [[ -n "${!var:-}" ]]; then
        return 0
    fi
    if [[ "$NONINTERACTIVE" == 1 ]]; then
        [[ -n "$default" ]] && { printf -v "$var" '%s' "$default"; export "${var?}"; return 0; }
        die "$var not set and --yes given (no prompt allowed)"
    fi
    if [[ -n "$silent" ]]; then
        read -r -s -p "$prompt_text${default:+ [$default]}: " val; echo
    else
        read -r -p "$prompt_text${default:+ [$default]}: " val
    fi
    [[ -z "$val" && -n "$default" ]] && val="$default"
    printf -v "$var" '%s' "$val"
    export "${var?}"
}

if [[ -f "$CONFIG_FILE" ]]; then
    log "preserving existing config: $CONFIG_FILE"
else
    log "generating config: $CONFIG_FILE"
    # Sensible default for interface: first up, non-loopback, non-wireless interface.
    DEFAULT_IFACE="$(ip -o link show up 2>/dev/null \
        | awk -F': ' '$2 !~ /^(lo|docker|br-|veth|wl)/ {print $2; exit}')"
    DEFAULT_IFACE="${DEFAULT_IFACE:-eno1}"
    DEFAULT_IP="$(ip -4 -o addr show dev "$DEFAULT_IFACE" 2>/dev/null \
        | awk '{print $4}' | cut -d/ -f1 | head -1)"

    prompt MK_INTERFACE  "Network interface for PXE (DHCP/TFTP/HTTP)" "$DEFAULT_IFACE"
    prompt MK_SERVER_IP  "Server IPv4 on $MK_INTERFACE"               "$DEFAULT_IP"
    prompt MK_HTTP_ADDR  "HTTP listen addr"                            ":8080"
    prompt MK_ADMIN_USER "Admin username"                              "admin"
    prompt MK_ADMIN_PASS "Admin password (empty = open mode, UNSAFE)"  "" silent

    [[ -z "$MK_SERVER_IP" ]] && die "MK_SERVER_IP empty — interface $MK_INTERFACE has no IPv4?"

    umask 027
    cat > "$CONFIG_FILE" <<EOF
# /etc/metalkit/config.yaml — generated by scripts/install.sh on $(date -Is)
serverIP: ${MK_SERVER_IP}
interface: ${MK_INTERFACE}
httpAddr: "${MK_HTTP_ADDR}"
dhcpAddr: ":67"
bsdpAddr: ":4011"
tftpAddr: ":69"
bootDir: ${PREFIX}/boot
logLevel: info

dbPath: ${DATA_DIR}/inventory.db
imagesDir: ${DATA_DIR}/images
masterKeyPath: ${DATA_DIR}/master.key

adminUser: ${MK_ADMIN_USER}
adminPass: "${MK_ADMIN_PASS}"
EOF
    chmod 0640 "$CONFIG_FILE"
fi

# ---------------------------------------------------------------------------
# 4. systemd unit
# ---------------------------------------------------------------------------

UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
log "installing systemd unit: $UNIT_FILE"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=metalkit bare-metal provisioning controller
Documentation=https://github.com/metalkit/metalkit
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=${PREFIX}/bin/metalkit-controller doctor -config ${CONFIG_FILE}
ExecStart=${PREFIX}/bin/metalkit-controller -config ${CONFIG_FILE}
# Privileges: needs CAP_NET_BIND_SERVICE (TFTP 69, DHCP 67) and root-only file
# perms on master.key — keep as root for now. Move to ambient caps + non-root
# user when /var/lib/metalkit ownership story is fully validated.
User=root
Group=root
WorkingDirectory=${PREFIX}
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal

# Light hardening (compatible with binding to privileged ports as root).
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ReadWritePaths=${DATA_DIR} ${PREFIX}/boot

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# 5. Doctor + restart
# ---------------------------------------------------------------------------

if [[ "$SKIP_DOCTOR" == 1 ]]; then
    log "skipping doctor (--skip-doctor)"
else
    log "running preflight: metalkit-controller doctor"
    if ! "${PREFIX}/bin/metalkit-controller" doctor -config "$CONFIG_FILE"; then
        warn "doctor reported failures — fix them before starting the service"
        warn "you can re-run:    ${PREFIX}/bin/metalkit-controller doctor -config ${CONFIG_FILE}"
        exit 1
    fi
fi

# (Re)start. Doctor-clean => safe to start.
log "restarting ${SERVICE_NAME}"
systemctl restart "$SERVICE_NAME" || die "service failed to start — journalctl -u ${SERVICE_NAME} -n 80"
sleep 1
systemctl is-active --quiet "$SERVICE_NAME" || die "service not active — journalctl -u ${SERVICE_NAME} -n 80"

cat <<EOF

$(printf '\033[1;32m[ok]\033[0m') metalkit controller installed and running.

  Config:    ${CONFIG_FILE}
  Data:      ${DATA_DIR}
  Logs:      journalctl -u ${SERVICE_NAME} -f
  Status:    systemctl status ${SERVICE_NAME}
  Doctor:    ${PREFIX}/bin/metalkit-controller doctor -config ${CONFIG_FILE}
  Web UI:    http://${MK_SERVER_IP:-<serverIP>}${MK_HTTP_ADDR:-:8080}/ui/

EOF
