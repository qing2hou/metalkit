package dhcp

import (
	"net"
	"net/netip"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// applyLeaseOptions adds the standard "I'm a real DHCP server" options to a
// reply: subnet mask (1), router (3), DNS (6), lease time (51), and the
// renew/rebind hints (58 = T1 = 0.5 * lease, 59 = T2 = 0.875 * lease).
// Server identifier (54) and class identifier (60) are set elsewhere because
// the PXE path also wants them.
//
// Mirrors what an ISC dhcpd or dnsmasq would emit for a standard lease;
// stripping any of these tends to confuse Windows guests or pickier
// initramfs DHCP clients.
func applyLeaseOptions(reply *dhcpv4.DHCPv4, pool *Pool) {
	reply.UpdateOption(dhcpv4.OptSubnetMask(net.IPMask(pool.Netmask.AsSlice())))
	reply.UpdateOption(dhcpv4.OptRouter(toIPv4(pool.Gateway)))
	if len(pool.DNS) > 0 {
		dns := make([]net.IP, 0, len(pool.DNS))
		for _, d := range pool.DNS {
			dns = append(dns, toIPv4(d))
		}
		reply.UpdateOption(dhcpv4.OptDNS(dns...))
	}
	lease := time.Duration(pool.LeaseSec) * time.Second
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(lease))
	reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRenewTimeValue, u32be(pool.LeaseSec/2)))
	reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRebindingTimeValue, u32be(pool.LeaseSec*7/8)))
}

func toIPv4(a netip.Addr) net.IP {
	b := a.As4()
	return net.IPv4(b[0], b[1], b[2], b[3]).To4()
}

func u32be(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
