//go:build linux

package functions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGuestRouteRequiresAssignedDefaultGateway(t *testing.T) {
	valid := "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\neth0 00000000 FDFEFEA9 0003 0 0 0 00000000 0 0 0\n"
	if !guestDefaultRouteReady(valid) {
		t.Fatal("own Runner default route rejected")
	}
	for _, invalid := range []string{"", strings.ReplaceAll(valid, "eth0", "lo"), strings.ReplaceAll(valid, "FDFEFEA9", "00000000"), strings.ReplaceAll(valid, "FDFEFEA9", "invalid"), strings.ReplaceAll(valid, "0003", "0001"), strings.ReplaceAll(valid, "0003", "nothex"), "eth0 00000000 FDFEFEA9 0003 0 0 0 00FFFFFF 0 0 0"} {
		if guestDefaultRouteReady(invalid) {
			t.Fatal("unready guest network accepted")
		}
	}
}

func TestGuestNetworkWaitObservesDHCPWithoutFixedSleep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	if err := awaitGuestNetwork(ctx, func() (bool, error) { calls++; return calls == 3, nil }, time.Millisecond); err != nil || calls != 3 {
		t.Fatal("DHCP transition not observed", err)
	}
	calls = 0
	if err := awaitGuestNetwork(ctx, func() (bool, error) { calls++; return true, nil }, time.Hour); err != nil || calls != 1 {
		t.Fatal("ready guest waited unnecessarily", err)
	}
}

func TestGuestNetworkWaitCancellationAndSafeObservationError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := awaitGuestNetwork(ctx, func() (bool, error) { return false, nil }, time.Millisecond); err == nil {
		t.Fatal("network wait ignored deadline")
	}
	err := awaitGuestNetwork(context.Background(), func() (bool, error) { return false, errors.New("private-observation-fixture") }, time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "private-observation-fixture") {
		t.Fatal("observation error accepted or disclosed")
	}
}
