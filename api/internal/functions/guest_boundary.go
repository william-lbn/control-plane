package functions

import (
	"errors"
	"net/netip"
)

// GuestBoundary applies inside one dedicated NeonVM. It must never run on a
// Kubernetes host: paths and network policy belong to the guest namespace.
// The exact own-fork kernel has IPv4 owner matching but no IPv6 filter table;
// IPv6 is therefore disabled in the guest before untrusted execution.
//
// 类型及纯参数校验由bootstrap契约跨平台引用，不能放在_linux.go而让Mac控制面
// 无法编译。此处仅搬移原类型/原校验，不执行任何guest操作；Prepare及防火墙、
// cgroup、设备操作仍限定Linux，不能把参数通过当作Mac具备这些隔离能力。
type GuestBoundary struct {
	ProxyIP          netip.Addr
	ProxyPort        uint16
	DNSIP            netip.Addr
	AllowPublicHTTPS bool
}

func (b GuestBoundary) Validate() error {
	if !b.ProxyIP.Is4() || !b.DNSIP.Is4() || b.ProxyIP.IsLoopback() || b.DNSIP.IsLoopback() || b.ProxyIP.IsUnspecified() || b.DNSIP.IsUnspecified() || b.ProxyPort == 0 {
		return errors.New("guest requires explicit IPv4 SQL proxy and DNS destinations")
	}
	return nil
}
