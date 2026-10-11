//go:build linux

package functions

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// AwaitGuestNetwork resolves an observed own-NeonVM boot race: inittab starts
// DHCP and vmstart concurrently. A static Go entry can sign/fetch before DHCP
// installs its route. Wait for the fixed guest interface/default route, with a
// hard deadline, instead of weakening TLS/auth or adding a fixed startup sleep.
// Only reads guest network state; never changes host clocks/routes or retries
// a customer invocation. The link-local IPv4 assigned by this Runner is valid.
func AwaitGuestNetwork(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("privileged function guest network prerequisite required")
	}
	marker, err := os.ReadFile("/etc/neon-function-guest")
	if err != nil || string(marker) != "isolated-neonvm-functions-v1\n" {
		return errors.New("function guest marker required")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return awaitGuestNetwork(waitCtx, func() (bool, error) {
		device, err := net.InterfaceByName("eth0")
		if err != nil || device.Flags&net.FlagUp == 0 {
			return false, nil
		}
		addresses, err := device.Addrs()
		if err != nil {
			return false, err
		}
		ipv4 := false
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil && prefix.Addr().Is4() && !prefix.Addr().IsUnspecified() && !prefix.Addr().IsLoopback() {
				ipv4 = true
			}
		}
		if !ipv4 {
			return false, nil
		}
		routes, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return false, err
		}
		return guestDefaultRouteReady(string(routes)), nil
	}, 100*time.Millisecond)
}

func guestDefaultRouteReady(routes string) bool {
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[0] != "eth0" || fields[1] != "00000000" || fields[2] == "00000000" || fields[7] != "00000000" {
			continue
		}
		gateway, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil || gateway == 0 {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 16)
		if err == nil && flags&3 == 3 { // RTF_UP | RTF_GATEWAY
			return true
		}
	}
	return false
}

func awaitGuestNetwork(ctx context.Context, observe func() (bool, error), interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return errors.New("function guest network readiness deadline exceeded")
		}
		ready, err := observe()
		if err != nil {
			return errors.New("function guest network readiness observation failed")
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("function guest network readiness deadline exceeded")
		case <-ticker.C:
		}
	}
}
