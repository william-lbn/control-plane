//go:build linux

package functions

import (
	"context"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestGuestEgressIsUIDScopedAndDefaultDeny(t *testing.T) {
	b := GuestBoundary{ProxyIP: netip.MustParseAddr("192.0.2.7"), ProxyPort: 30432, DNSIP: netip.MustParseAddr("10.43.0.10")}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	rules := b.guestRules()
	if !reflect.DeepEqual(rules[len(rules)-1], []string{"-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "65532", "-j", "NEON_FUNCTION"}) {
		t.Fatal("untrusted child is not isolated by UID")
	}
	if !reflect.DeepEqual(rules[len(rules)-2], []string{"-A", "NEON_FUNCTION", "-j", "DROP"}) {
		t.Fatal("egress does not fail closed")
	}
	for _, rule := range rules {
		if strings.Contains(strings.Join(rule, " "), "--dport 6443") {
			t.Fatal("Kubernetes control API allow appeared")
		}
	}
	b.AllowPublicHTTPS = true
	rules = b.guestRules()
	privateBlock, publicAllow := -1, -1
	for i, rule := range rules {
		text := strings.Join(rule, " ")
		if strings.Contains(text, "169.254.0.0/16") {
			privateBlock = i
		}
		if strings.Contains(text, "--dport 443 -j ACCEPT") {
			publicAllow = i
		}
	}
	if privateBlock < 0 || publicAllow <= privateBlock {
		t.Fatal("public HTTPS can bypass link-local/metadata rejection")
	}
}

func TestGuestDestinationsRejectAmbiguousOrUnsupportedAddresses(t *testing.T) {
	for _, b := range []GuestBoundary{{}, {ProxyIP: netip.MustParseAddr("::1"), DNSIP: netip.MustParseAddr("10.43.0.10"), ProxyPort: 5432}, {ProxyIP: netip.MustParseAddr("127.0.0.1"), DNSIP: netip.MustParseAddr("10.43.0.10"), ProxyPort: 5432}, {ProxyIP: netip.MustParseAddr("192.0.2.7"), DNSIP: netip.MustParseAddr("10.43.0.10")}} {
		if b.Validate() == nil {
			t.Fatal("invalid guest destination accepted")
		}
	}
}

func TestGuestChildNeverInheritsManagerOrHostEnvironment(t *testing.T) {
	file, err := os.Open("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	g := &ChildGroup{path: "/sys/fs/cgroup/neon-function", file: file}
	t.Setenv("NEON_MANAGER_KEY", "host-secret-never-inherit")
	command, err := g.Command(context.Background(), "/opt/neon/functions/runtime/boot.mjs", map[string]string{"APP_TOKEN": "branch-secret"}, "postgresql://restricted@proxy/db")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Env, "\n")
	if strings.Contains(joined, "host-secret-never-inherit") || !strings.Contains(joined, "APP_TOKEN=branch-secret") || !strings.Contains(joined, "DATABASE_URL=") {
		t.Fatal("child environment boundary failed")
	}
	if !command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD != int(file.Fd()) || command.Cancel == nil {
		t.Fatal("child can escape resource/cleanup boundary")
	}
	if !strings.Contains(strings.Join(command.Args, " "), "--no-new-privs --bounding-set=-all --inh-caps=-all --ambient-caps=-all") {
		t.Fatal("child capability boundary missing")
	}
	if _, err = g.Command(context.Background(), "/bin/sh", map[string]string{}, "dsn"); err == nil {
		t.Fatal("arbitrary command entry accepted")
	}
}
