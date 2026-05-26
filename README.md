# metalkit

Single-binary bare-metal PXE provisioning controller. Bundles a ProxyDHCP, TFTP, and HTTP server to PXE-boot bare-metal hosts into a Debian live system over the network.

## Status: M1 — boot chain only

This milestone delivers the cold-boot chain: client PXE ROM → ProxyDHCP → TFTP (iPXE) → iPXE second-stage DHCP → HTTP (iPXE script + kernel + initrd + squashfs) → Debian live root shell.

**Out of scope for M1**: installer agent inside the live image, OS image writing, cloud-init injection, IPMI/Redfish, SQLite state, Web UI. See the plan file for the full M1→M7 roadmap.

## Prerequisites

- Go ≥ 1.23 (the `insomniacslk/dhcp` library requires it; if your system Go is older, `GOTOOLCHAIN=auto` will fetch the right one)
- Docker (for the Debian live-image build; live-build needs a Debian/Ubuntu host)
- `curl` (for fetching iPXE binaries)
- Linux host with `CAP_NET_BIND_SERVICE` or root to bind UDP 67/69/4011 and TCP 80/8080

## Build

```
make tidy        # resolve Go deps
make ipxe        # fetch undionly.kpxe / snponly.efi / ipxe.efi → internal/ipxebin/assets/
make build       # produce bin/controller (~8MB, CGO-free, static)
make live        # build Debian live image via docker → boot/{vmlinuz,initrd.img,filesystem.squashfs}
                 # (10–25 min, downloads ~600MB of apt packages)
```

If `make` invokes the wrong Go: `make GO=/path/to/go build`.

The iPXE binaries are pulled fresh from `boot.ipxe.org` by `scripts/fetch-ipxe.sh`. The script prints SHA-256 hashes — to make builds reproducible, copy them into the script and add a verification step.

## Run

Edit `config.example.yaml` (or create your own) and set `serverIP` to the IP your control plane will bind. **Must be an IPv4 literal**: live-boot's initramfs busybox-wget does not resolve DNS, so the URL in the iPXE script must be a numeric IP.

```yaml
serverIP: 10.99.0.1           # IPv4 literal; clients fetch boot files from this IP
interface: br-pxe             # network interface to bind DHCP/TFTP (Linux SO_BINDTODEVICE)
httpAddr: ":8080"
dhcpAddr: ":67"
bsdpAddr: ":4011"             # PXE Boot Server Discovery (Layer 3), used by strict ROMs (Dell)
tftpAddr: ":69"
bootDir: ./boot               # must contain vmlinuz, initrd.img, filesystem.squashfs
logLevel: info                # debug|info|warn|error
```

```
sudo ./bin/controller -config config.example.yaml
```

Logs are structured JSON on stderr. SIGTERM/SIGINT triggers graceful shutdown (5s timeout).

## End-to-end test with QEMU

Tested in an isolated bridge — **do not run this on your office LAN**, it will start replying to PXE clients.

1. **Create an isolated bridge** (Linux):
   ```
   sudo ip link add br-pxe type bridge
   sudo ip addr add 10.99.0.1/24 dev br-pxe
   sudo ip link set br-pxe up
   ```

2. **Start dnsmasq for IP assignment only** (no PXE options — those come from metalkit):
   ```
   sudo dnsmasq \
     --interface=br-pxe --bind-interfaces \
     --dhcp-range=10.99.0.100,10.99.0.200,12h \
     --port=0 --no-daemon
   ```
   `--port=0` disables DNS, leaving only DHCP. Coexisting with metalkit's ProxyDHCP on 67 works because dnsmasq does not set option 60.

3. **Build everything** (see Build above), then start metalkit:
   ```
   sudo ./bin/controller -config config.example.yaml
   ```

4. **Allow QEMU bridge helper** (one-time host setup):
   ```
   sudo install -d /etc/qemu
   echo 'allow br-pxe' | sudo tee /etc/qemu/bridge.conf
   sudo chmod u+s /usr/lib/qemu/qemu-bridge-helper   # path varies per distro
   ```

5. **Boot a BIOS PXE client**:
   ```
   qemu-system-x86_64 -enable-kvm -m 2048 -boot n -nographic \
     -netdev bridge,id=n0,br=br-pxe \
     -device e1000,netdev=n0,mac=52:54:00:12:34:56
   ```

6. **Boot a UEFI PXE client** (add OVMF firmware):
   ```
   qemu-system-x86_64 -enable-kvm -m 2048 -boot n -nographic \
     -bios /usr/share/OVMF/OVMF_CODE.fd \
     -netdev bridge,id=n0,br=br-pxe \
     -device e1000,netdev=n0,mac=52:54:00:12:34:57
   ```

Expected timeline (~1–2 min on a fast link):
- PXE ROM gets IP from dnsmasq
- PXE ROM gets ProxyDHCP offer from metalkit pointing at `undionly.kpxe` (BIOS) or `snponly.efi` (UEFI x64)
- TFTP fetch of the iPXE binary
- iPXE re-does DHCP — metalkit identifies it via DHCP option 77=`iPXE` and responds with `http://<serverIP>:8080/boot/ipxe` as filename
- iPXE chainloads the HTTP script; `kernel`/`initrd` lines pull from HTTP
- Kernel boots, live-boot initramfs runs `fetch=` to pull squashfs (~800MB) into RAM
- Debian login prompt appears (no agent yet — just `root` shell via getty on tty1)

## How the boot chain works

```
client PXE ROM
   │ option 60 = PXEClient:Arch:NNNN:...
   │ option 93 = client architecture
   ▼
[dnsmasq]              gives an IP (yiaddr) only
[metalkit ProxyDHCP]   minimal advertisement on UDP 67:
                       msg=Offer/ACK, Server-ID, option 60=PXEClient, GUID echo.
                       NO bootfile/siaddr/option 66 in this packet — strict PXE
                       ROMs (Dell PowerEdge) reject any port-67 reply that
                       includes them. This advert just tells the client
                       "a PXE boot server lives at this IP".
   │
   ▼
[metalkit BSDP on UDP 4011]   client unicasts a DHCPRequest to <serverIP>:4011
                              after getting its IP; metalkit answers with
                              msg=ACK, siaddr=serverIP, sname=serverIP,
                              file = bootfile for arch.
                              (iPXE in QEMU does not need this — it accepts
                              Layer 1 on port 67; only strict ROMs do BSDP.)
   │
   ▼
TFTP fetch from siaddr:69
   • undionly.kpxe   (BIOS,         arch 0x0000)
   • snponly.efi     (UEFI x86_64,  arch 0x0007 or 0x0009)
   • ipxe.efi        (UEFI fallback for older firmware)
   │
   ▼
iPXE runs, re-does DHCP on port 67 (Layer 1, no BSDP)
   │ option 60 = PXEClient (unchanged!)
   │ option 77 = iPXE        ← metalkit uses THIS to detect 2nd stage
   ▼
[metalkit ProxyDHCP]   answers on port 67 with filename = "http://10.99.0.1:8080/boot/ipxe"
   │
   ▼
iPXE chainloads HTTP script:
   kernel  http://10.99.0.1:8080/boot/vmlinuz initrd=initrd.img boot=live fetch=http://10.99.0.1:8080/boot/filesystem.squashfs ip=dhcp
   initrd  http://10.99.0.1:8080/boot/initrd.img
   boot
   │
   ▼
Linux boot, live-boot initramfs:
   • ip=dhcp gets an address
   • fetch=URL downloads squashfs into RAM
   • pivots root to the live filesystem
   ▼
Debian login prompt (M2 will replace this with installer-agent)
```

## Configuration reference

| Field | Required | Description |
|---|---|---|
| `serverIP` | yes | IPv4 literal. Embedded into iPXE script (`fetch=http://IP/...`) — must be numeric, live-boot's initramfs does not resolve DNS. |
| `interface` | yes | Network interface to bind DHCP/TFTP (e.g. `br-pxe`, `eth0`). Used with `SO_BINDTODEVICE`. |
| `httpAddr` | yes | TCP listen for HTTP, e.g. `:8080` or `10.99.0.1:8080`. |
| `dhcpAddr` | yes | UDP listen for ProxyDHCP, typically `:67`. |
| `bsdpAddr` | no | UDP listen for PXE Boot Server Discovery (Layer 3), default `:4011`. Required for strict PXE ROMs (Dell PowerEdge, some HP). |
| `tftpAddr` | yes | UDP listen for TFTP, typically `:69`. |
| `bootDir` | yes | Filesystem directory with `vmlinuz`, `initrd.img`, `filesystem.squashfs` produced by `make live`. |
| `logLevel` | no | `debug` / `info` (default) / `warn` / `error`. |

## Repository layout

```
cmd/controller/        # main entry point, wires the four servers
internal/config/       # YAML loader + validation
internal/dhcp/         # ProxyDHCP (UDP 67) + BSDP Layer 3 (UDP 4011), insomniacslk/dhcp/dhcpv4
internal/tftp/         # TFTP read-only, pin/tftp/v3
internal/httpd/        # HTTP + iPXE script template, stdlib only
internal/ipxebin/      # iPXE binaries embedded via //go:embed assets
live-image/            # Debian live-build project (auto/, config/)
scripts/
  fetch-ipxe.sh        # downloads undionly.kpxe / snponly.efi / ipxe.efi
  build-live.sh        # runs lb build inside a privileged debian:bookworm container
```

## Known limitations / gotchas

- **Secure Boot must be disabled on target hosts.** The iPXE binaries from `boot.ipxe.org` are not signed with Microsoft's UEFI CA. Disable Secure Boot in BMC before PXE install.
- **The `fetch=` URL must use an IPv4 literal.** The constructor enforces this; if you set a hostname in `serverIP`, the controller refuses to start.
- **ProxyDHCP coexistence**: assumes a separate DHCP server is handing out IPs. If you run metalkit on a segment with no DHCP at all, clients won't get an IP and PXE never starts. (Full DHCP support is a future feature.)
- **First DHCP must not set option 60=PXEClient.** dnsmasq does not, by default. ISC dhcpd and some enterprise DHCPs do — if you see clients trying to chainload from your IT DHCP, configure that DHCP to skip option 60 or run metalkit on a dedicated VLAN.
- **arm64 (option 93 = 0x000b)** is recognized but the iPXE binary isn't included in `make ipxe`. Add `arm64-snponly.efi` to `scripts/fetch-ipxe.sh` from `https://boot.ipxe.org/arm64-efi/` if you need it.
- **No SHA-256 pinning on iPXE binaries yet.** `scripts/fetch-ipxe.sh` prints the hashes; pin them in the script for reproducibility.

## Tests

```
go test ./...            # 22 tests across dhcp/tftp/httpd
go test -race ./...      # race detector (passes)
go vet ./...
```

## End-to-end build & deploy guide (Chinese)

For the full M1 build → deploy → real-hardware-verify procedure, including all the
hook customizations baked into the live image, the dnsmasq sidecar setup, and the
field-tested troubleshooting playbook, see [`docs/build-and-deploy.md`](docs/build-and-deploy.md).

## Plan

Full design and milestone breakdown: `~/.claude/plans/buzzing-leaping-scroll.md` (M1 only). Subsequent milestones (installer agent, multi-distro OS install, network config, IPMI/Redfish) are scoped but not yet planned in detail.
