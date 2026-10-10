//go:build linux

package functions

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// HardenGuestCgroups overrides the cooperative (world-writable root procs)
// defaults in the own-fork NeonVM init. Untrusted Node must never migrate itself
// out of its resource group. Run only after the guest marker/root check.
func HardenGuestCgroups() error {
	if os.Geteuid() != 0 {
		return errors.New("privileged guest cgroup hardening required")
	}
	marker, err := os.ReadFile("/etc/neon-function-guest")
	if err != nil || string(marker) != "isolated-neonvm-functions-v1\n" {
		return errors.New("function guest marker required")
	}
	for _, path := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/cgroup.procs", "/sys/fs/cgroup/cgroup.threads"} {
		mode := os.FileMode(0600)
		if path == "/sys/fs/cgroup" {
			mode = 0755
		}
		if err := os.Chmod(path, mode); err != nil {
			return errors.New("guest cgroup migration boundary unavailable")
		}
	}
	if err := os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory +pids"), 0600); err != nil {
		return errors.New("guest resource controllers unavailable")
	}
	return nil
}

// HideGuestBlockDevices denies raw reads of the detached Secret CD-ROM as well
// as runtime/root disks. The Functions rootfs also installs a root-only udev
// block rule; hotplug is unsupported for this fixed-resource instance contract.
func HideGuestBlockDevices() error {
	if os.Geteuid() != 0 {
		return errors.New("privileged block device hardening required")
	}
	return filepath.WalkDir("/dev", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return errors.New("guest device enumeration failed")
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return errors.New("guest device inspection failed")
		}
		if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
			if os.Chown(path, 0, 0) != nil || os.Chmod(path, 0600) != nil {
				return errors.New("raw guest device boundary unavailable")
			}
		}
		return nil
	})
}

// GuestBoundary applies inside one dedicated NeonVM. It must never run on a
// Kubernetes host: paths and network policy belong to the guest namespace.
// The exact own-fork kernel has IPv4 owner matching but no IPv6 filter table;
// IPv6 is therefore disabled in the guest before untrusted execution.
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

// guestRules are argument arrays, never interpolated shell text. The terminal
// owner rule rejects every unapproved destination, including metadata IPs and
// Kubernetes control/management APIs even when a VXLAN path bypasses CNI policy.
func (b GuestBoundary) guestRules() [][]string {
	rules := [][]string{
		{"-N", "NEON_FUNCTION"},
		{"-A", "NEON_FUNCTION", "-d", "127.0.0.0/8", "-j", "ACCEPT"},
		{"-A", "NEON_FUNCTION", "-d", b.ProxyIP.String(), "-p", "tcp", "--dport", strconv.Itoa(int(b.ProxyPort)), "-j", "ACCEPT"},
		{"-A", "NEON_FUNCTION", "-d", b.DNSIP.String(), "-p", "udp", "--dport", "53", "-j", "ACCEPT"},
		{"-A", "NEON_FUNCTION", "-d", b.DNSIP.String(), "-p", "tcp", "--dport", "53", "-j", "ACCEPT"},
	}
	if b.AllowPublicHTTPS {
		// Test private ranges before the optional public HTTPS allow. DNS names
		// resolving to private, link-local, multicast or reserved destinations
		// remain rejected on every packet, not just initial DNS resolution.
		for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"} {
			rules = append(rules, []string{"-A", "NEON_FUNCTION", "-d", cidr, "-j", "REJECT"})
		}
		rules = append(rules, []string{"-A", "NEON_FUNCTION", "-p", "tcp", "--dport", "443", "-j", "ACCEPT"})
	}
	rules = append(rules, []string{"-A", "NEON_FUNCTION", "-j", "REJECT"}, []string{"-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "65532", "-j", "NEON_FUNCTION"})
	return rules
}

func (b GuestBoundary) Prepare(ctx context.Context) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("guest boundary requires privileged supervisor")
	}
	// Require the immutable VM build marker and exclude a containerized/host
	// execution by checking it before touching sysctls or netfilter.
	marker, err := os.ReadFile("/etc/neon-function-guest")
	if err != nil || string(marker) != "isolated-neonvm-functions-v1\n" {
		return errors.New("function guest marker required")
	}
	paths, err := filepath.Glob("/proc/sys/net/ipv6/conf/*/disable_ipv6")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err = os.WriteFile(path, []byte("1\n"), 0600); err != nil {
			return errors.New("guest IPv6 isolation could not be established")
		}
	}
	for _, args := range b.guestRules() {
		command := exec.CommandContext(ctx, "/sbin/iptables", append([]string{"-w", "5"}, args...)...)
		if err = command.Run(); err != nil {
			return errors.New("guest network boundary could not be established")
		}
	}
	return nil
}

type ChildGroup struct {
	path string
	file *os.File
}

// NewChildGroup reserves 1536 MiB for Node inside the documented 2048 MiB
// isolate, leaving supervisor/guest overhead. The process belongs to its cgroup
// from fork, so a child cannot escape cleanup by creating another session.
func NewChildGroup() (*ChildGroup, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("guest cgroup requires privileged supervisor")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return nil, errors.New("cgroup v2 is required")
	}
	path := "/sys/fs/cgroup/neon-function"
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, errors.New("fresh isolated child cgroup required")
	}
	for name, value := range map[string]string{"memory.max": "1610612736", "memory.swap.max": "0", "pids.max": "64"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0600); err != nil {
			return nil, errors.New("required child resource boundary is unavailable")
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &ChildGroup{path: path, file: file}, nil
}

func (g *ChildGroup) Command(ctx context.Context, entry string, environment map[string]string, databaseURL string) (*exec.Cmd, error) {
	if err := ValidateEnvironment(environment); err != nil {
		return nil, err
	}
	if entry != "/opt/neon/functions/runtime/boot.mjs" || databaseURL == "" || strings.ContainsRune(databaseURL, 0) {
		return nil, errors.New("fixed runtime entry and branch SQL credential required")
	}
	command := exec.CommandContext(ctx, "/usr/bin/setpriv", "--reuid=65532", "--regid=65532", "--clear-groups", "--no-new-privs", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "/usr/local/bin/node", "--max-old-space-size=1024", entry)
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(g.file.Fd()), Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Dir = "/srv/function"
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/srv/function", "TMPDIR=/tmp/function", "NODE_ENV=production", "DATABASE_URL=" + databaseURL}
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	// Context cancellation kills the entire tree, including detached user
	// subprocesses. A graceful SIGINT phase is owned by the supervisor caller.
	command.Cancel = g.Kill
	return command, nil
}

func (g *ChildGroup) Kill() error {
	if g == nil || g.path != "/sys/fs/cgroup/neon-function" {
		return errors.New("invalid child group")
	}
	return os.WriteFile(filepath.Join(g.path, "cgroup.kill"), []byte("1"), 0600)
}

func (g *ChildGroup) Close() error {
	if g == nil || g.file == nil {
		return errors.New("invalid child group")
	}
	return g.file.Close()
}

// DetachBootstrap removes the secret CD-ROM before Node starts. It is a hard
// boot prerequisite; permissions on an exposed read-only Secret are inadequate.
func DetachBootstrap(mount string) error {
	if mount != "/run/function-bootstrap" || os.Geteuid() != 0 {
		return errors.New("invalid bootstrap mount")
	}
	if err := syscall.Unmount(mount, 0); err != nil {
		return fmt.Errorf("bootstrap secret detach failed: %w", err)
	}
	if entries, err := os.ReadDir(mount); err != nil || len(entries) != 0 {
		return errors.New("bootstrap contents remain visible after detach")
	}
	return os.Chmod(mount, 0700)
}
