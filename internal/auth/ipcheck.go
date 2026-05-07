package auth

import (
	"net"
	"strings"
)

// CheckIP validates that clientIP is allowed by the whitelist.
// Empty whitelist = allow all. Supports CIDR notation and plain IPs.
func CheckIP(clientIP, whitelist string) bool {
	if whitelist == "" {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	for _, entry := range strings.Split(whitelist, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err == nil && cidr.Contains(ip) {
				return true
			}
		} else {
			if net.ParseIP(entry) != nil && entry == ip.String() {
				return true
			}
		}
	}
	return false
}
