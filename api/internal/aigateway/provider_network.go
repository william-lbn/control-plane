package aigateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type ApprovedProviderEndpoint struct {
	BaseURL    string   `json:"base_url"`
	AllowedIPs []string `json:"allowed_ips"`
	LabHTTP    bool     `json:"lab_allow_http"`
}
type ResolvedProviderEndpoint struct {
	BaseURL        string   `json:"base_url"`
	AllowedIPs     []string `json:"allowed_ips"`
	LabHTTP        bool     `json:"lab_allow_http"`
	EndpointSHA256 string   `json:"endpoint_sha256"`
}
type ProviderNetworkPolicy struct {
	approved    map[string]ApprovedProviderEndpoint
	lookup      func(context.Context, string) ([]netip.Addr, error)
	egressProxy string
}

var errProviderEndpoint = errors.New("provider endpoint or resolved address rejected")

func CanonicalProviderURL(value string) (string, error) {
	base, _, _, _, err := canonicalProviderEndpoint(value)
	return base, err
}

func (p *ProviderNetworkPolicy) IsApprovedLabEndpoint(value string) bool {
	if p == nil || p.lookup == nil {
		return false
	}
	_, origin, _, _, err := canonicalProviderEndpoint(value)
	if err != nil || !strings.HasPrefix(origin, "http://") {
		return false
	}
	entry, ok := p.approved[origin]
	if !ok || !entry.LabHTTP {
		return false
	}
	for _, raw := range entry.AllowedIPs {
		ip, err := netip.ParseAddr(raw)
		if err != nil || providerAddressAlwaysDenied(ip) || providerPublicAddress(ip) {
			return false
		}
	}
	return len(entry.AllowedIPs) > 0
}

var providerDeniedNetworks = func() []netip.Prefix {
	values := []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2001::/23", "2002::/16", "fec0::/10", "fd00:ec2::254/128"}
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}()

func providerAddressAlwaysDenied(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.Zone() != "" {
		return true
	}
	for _, prefix := range providerDeniedNetworks {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
func providerPublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !providerAddressAlwaysDenied(ip) && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback()
}

func canonicalProviderEndpoint(value string) (base, origin, host, port string, err error) {
	if len(value) == 0 || len(value) > 2048 || strings.TrimSpace(value) != value {
		return "", "", "", "", errProviderEndpoint
	}
	u, e := url.Parse(value)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "/v1" && u.Path != "/v1/") {
		return "", "", "", "", errProviderEndpoint
	}
	host = strings.ToLower(u.Hostname())
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "%\\\x00") {
		return "", "", "", "", errProviderEndpoint
	}
	if ip, e := netip.ParseAddr(host); e == nil {
		host = ip.Unmap().String()
	} else {
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", "", "", "", errProviderEndpoint
			}
			for _, b := range []byte(label) {
				if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-') {
					return "", "", "", "", errProviderEndpoint
				}
			}
		}
	}
	port = u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return "", "", "", "", errProviderEndpoint
	}
	port = strconv.Itoa(n)
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if !(u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80") {
		authority = net.JoinHostPort(host, port)
	}
	origin = u.Scheme + "://" + authority
	base = origin + "/v1"
	return
}

// Private approval is an exact origin/port plus fixed addresses. It is never
// interpreted as permission to contact every service on the approved node.
func NewProviderNetworkPolicy(entries []ApprovedProviderEndpoint) (*ProviderNetworkPolicy, error) {
	if len(entries) > 32 {
		return nil, errProviderEndpoint
	}
	p := &ProviderNetworkPolicy{approved: map[string]ApprovedProviderEndpoint{}}
	p.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	for _, entry := range entries {
		base, origin, _, _, err := canonicalProviderEndpoint(entry.BaseURL)
		if err != nil || len(entry.AllowedIPs) < 1 || len(entry.AllowedIPs) > 8 || strings.HasPrefix(base, "http:") && !entry.LabHTTP {
			return nil, errProviderEndpoint
		}
		if _, exists := p.approved[origin]; exists {
			return nil, errProviderEndpoint
		}
		ips := map[string]bool{}
		for _, raw := range entry.AllowedIPs {
			ip, err := netip.ParseAddr(raw)
			if err != nil || providerAddressAlwaysDenied(ip) {
				return nil, errProviderEndpoint
			}
			ips[ip.Unmap().String()] = true
		}
		entry.BaseURL = base
		entry.AllowedIPs = nil
		for ip := range ips {
			entry.AllowedIPs = append(entry.AllowedIPs, ip)
		}
		sort.Strings(entry.AllowedIPs)
		p.approved[origin] = entry
	}
	return p, nil
}

func (p *ProviderNetworkPolicy) allow(origin string, ip netip.Addr, scheme, port string) bool {
	if providerAddressAlwaysDenied(ip) {
		return false
	}
	if entry, approved := p.approved[origin]; approved {
		if scheme == "http" && !entry.LabHTTP {
			return false
		}
		for _, allowed := range entry.AllowedIPs {
			if ip.Unmap().String() == allowed {
				return true
			}
		}
		return false
	}
	return scheme == "https" && port == "443" && providerPublicAddress(ip)
}

func (p *ProviderNetworkPolicy) Resolve(ctx context.Context, value string) (ResolvedProviderEndpoint, error) {
	var empty ResolvedProviderEndpoint
	if p == nil || p.lookup == nil {
		return empty, errProviderEndpoint
	}
	base, origin, host, port, err := canonicalProviderEndpoint(value)
	if err != nil {
		return empty, err
	}
	var ips []netip.Addr
	if literal, e := netip.ParseAddr(host); e == nil {
		ips = []netip.Addr{literal}
	} else {
		ips, err = p.lookup(ctx, host)
		if err != nil {
			return empty, errProviderEndpoint
		}
	}
	if len(ips) == 0 || len(ips) > 8 {
		return empty, errProviderEndpoint
	}
	u, _ := url.Parse(base)
	unique := map[string]bool{}
	for _, ip := range ips {
		if !p.allow(origin, ip, u.Scheme, port) {
			return empty, errProviderEndpoint
		}
		unique[ip.Unmap().String()] = true
	}
	result := ResolvedProviderEndpoint{BaseURL: base, LabHTTP: u.Scheme == "http"}
	for ip := range unique {
		result.AllowedIPs = append(result.AllowedIPs, ip)
	}
	sort.Strings(result.AllowedIPs)
	hash := sha256.Sum256([]byte(base))
	result.EndpointSHA256 = hex.EncodeToString(hash[:])
	return result, nil
}

func (p *ProviderNetworkPolicy) ValidateResolved(value ResolvedProviderEndpoint) error {
	if p == nil || p.lookup == nil || len(value.AllowedIPs) < 1 || len(value.AllowedIPs) > 8 {
		return errProviderEndpoint
	}
	base, origin, _, port, err := canonicalProviderEndpoint(value.BaseURL)
	if err != nil || base != value.BaseURL {
		return errProviderEndpoint
	}
	u, _ := url.Parse(base)
	hash := sha256.Sum256([]byte(base))
	if value.EndpointSHA256 != hex.EncodeToString(hash[:]) || value.LabHTTP != (u.Scheme == "http") {
		return errProviderEndpoint
	}
	seen := map[string]bool{}
	for _, raw := range value.AllowedIPs {
		ip, err := netip.ParseAddr(raw)
		if err != nil || seen[ip.Unmap().String()] || !p.allow(origin, ip, u.Scheme, port) {
			return errProviderEndpoint
		}
		seen[ip.Unmap().String()] = true
	}
	return nil
}
