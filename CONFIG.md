# Configuration Guide

## Auto-detecting Server IP

Starting from this version, `serverIP` is **optional** in the configuration file. If not specified, metalkit will automatically detect the primary IPv4 address from the specified network interface.

### Minimal Configuration

```yaml
interface: eth0           # Your network interface (required)
httpAddr: ":8080"
dhcpAddr: ":67"
bsdpAddr: ":4011"
tftpAddr: ":69"
bootDir: /opt/metalkit/boot
logLevel: info
dbPath: /var/lib/metalkit/inventory.db
imagesDir: /var/lib/metalkit/images
masterKeyPath: /var/lib/metalkit/master.key
adminUser: admin
adminPass: metalkit
dhcpMode: full
```

When metalkit starts, it will:
1. Detect the primary IPv4 address on the specified interface
2. Log the detected IP: `auto-detected serverIP interface=eth0 ip=192.168.10.120`
3. Use this IP for DHCP/TFTP/HTTP services

### Explicit Server IP (Optional)

If you want to explicitly specify the server IP (e.g., when the interface has multiple IPs):

```yaml
serverIP: 192.168.10.120  # Explicit IP address
interface: eth0
# ... rest of config
```

## DHCP Modes

### Proxy Mode (Default)

Requires an external DHCP server to allocate IP addresses. Metalkit only provides PXE boot information.

```yaml
dhcpMode: proxy  # or omit this line (defaults to proxy)
```

### Full Mode

Metalkit acts as a complete DHCP server, allocating IP addresses and providing PXE boot information.

```yaml
dhcpMode: full

# Optional: DHCP pool configuration
# If not specified, metalkit will auto-detect from the interface
dhcpPool:
  start: 192.168.10.100      # First IP to lease
  end: 192.168.10.200        # Last IP to lease
  netmask: 255.255.255.0     # Auto-detected if omitted
  gateway: 192.168.10.1      # Auto-detected if omitted (subnet base + 1)
  dns:                       # Defaults to [8.8.8.8, 1.1.1.1]
    - 8.8.8.8
    - 1.1.1.1
  leaseHours: 24             # Defaults to 24
  exclude:                   # IPs to never lease (serverIP and gateway auto-added)
    - 192.168.10.1
    - 192.168.10.50
```

## Deployment on Different Networks

With auto-detection, you can use the **same configuration file** across different networks:

1. Copy `config.auto.yaml` to your target server
2. Edit only the `interface` field to match your network interface
3. Start metalkit - it will automatically detect and use the correct IP

Example for different servers:
- Server A (eth0 = 192.168.10.120) → auto-detects 192.168.10.120
- Server B (ens32 = 10.0.0.50) → auto-detects 10.0.0.50
- Server C (enp0s3 = 172.16.1.100) → auto-detects 172.16.1.100

## Finding Your Network Interface

```bash
# List all network interfaces
ip addr show

# Or use
ip link show
```

Look for the interface with your network's IP address (not `lo` or `docker0`).
