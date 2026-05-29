#!/bin/bash
set -e

# Deployment script for metalkit
# Usage: ./deploy.sh <target-server-ip> [ssh-user]

TARGET_SERVER="${1:-192.168.10.120}"
SSH_USER="${2:-root}"
BINARY_PATH="./metalkit"
REMOTE_INSTALL_PATH="/usr/local/bin/metalkit"
SERVICE_NAME="metalkit"

echo "=== Metalkit Deployment Script ==="
echo "Target server: ${SSH_USER}@${TARGET_SERVER}"
echo "Local binary: ${BINARY_PATH}"
echo "Remote path: ${REMOTE_INSTALL_PATH}"
echo ""

# Check if binary exists
if [ ! -f "${BINARY_PATH}" ]; then
    echo "Error: Binary not found at ${BINARY_PATH}"
    exit 1
fi

echo "Step 1: Copying binary to target server..."
scp "${BINARY_PATH}" "${SSH_USER}@${TARGET_SERVER}:/tmp/metalkit-new" || {
    echo "Error: Failed to copy binary. Please check SSH access."
    exit 1
}

echo "Step 2: Deploying on target server..."
ssh "${SSH_USER}@${TARGET_SERVER}" << 'ENDSSH'
set -e

echo "  - Stopping metalkit service..."
if systemctl is-active --quiet metalkit 2>/dev/null; then
    systemctl stop metalkit
    echo "    Service stopped"
elif pgrep -f metalkit > /dev/null; then
    echo "    Killing metalkit process..."
    pkill -f metalkit || true
    sleep 2
else
    echo "    Service not running"
fi

echo "  - Backing up old binary..."
if [ -f /usr/local/bin/metalkit ]; then
    cp /usr/local/bin/metalkit /usr/local/bin/metalkit.backup.$(date +%Y%m%d_%H%M%S)
    echo "    Backup created"
fi

echo "  - Installing new binary..."
mv /tmp/metalkit-new /usr/local/bin/metalkit
chmod +x /usr/local/bin/metalkit
echo "    Binary installed"

echo "  - Starting metalkit service..."
if systemctl list-unit-files | grep -q metalkit.service; then
    systemctl start metalkit
    sleep 2
    systemctl status metalkit --no-pager || true
else
    echo "    No systemd service found. Please start metalkit manually."
fi

echo ""
echo "Deployment completed successfully!"
echo "Binary version:"
/usr/local/bin/metalkit -version 2>&1 || echo "  (version flag not available)"
ENDSSH

echo ""
echo "=== Deployment Complete ==="
echo "Please verify the service is running correctly."
