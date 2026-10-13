package harvest

import (
	"fmt"
	"net/netip"
)

// Edge IP 只接受公网字面地址；实际请求仍校验原上游域名的证书。
func ValidateEdgeIP(value string) error {
	if value == "" {
		return nil
	}
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return fmt.Errorf("边缘 IP 必须是公网 IP 地址")
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return fmt.Errorf("边缘 IP 必须是公网 IP 地址")
	}
	for _, prefix := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "100::/64", "2001:db8::/32"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return fmt.Errorf("边缘 IP 必须是公网 IP 地址")
		}
	}
	return nil
}
