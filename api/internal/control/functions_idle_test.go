package control

import (
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func TestFunctionIdleUnhealthyNodeUsesOnlyTrustedIdleCounters(t *testing.T) {
	now := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	base := functions.InstanceStatus{LastActivity: now.Add(-31 * time.Second)}
	for _, healthy := range []bool{false, true} {
		status := base
		status.NodeReady = healthy
		if !functionIdleEligible(status, 30, now) {
			t.Fatal("original idle supervisor refused because of customer Node health")
		}
		status.ActiveInvocations = 1
		if functionIdleEligible(status, 30, now) {
			t.Fatal("active stream retired")
		}
	}
	for _, change := range []func(*functions.InstanceStatus){
		func(s *functions.InstanceStatus) { s.Draining = true },
		func(s *functions.InstanceStatus) { s.LastActivity = time.Time{} },
		func(s *functions.InstanceStatus) { s.LastActivity = now.Add(time.Second) },
		func(s *functions.InstanceStatus) { s.LastActivity = now.Add(-29 * time.Second) },
		func(s *functions.InstanceStatus) { s.ActiveInvocations = -1 },
		func(s *functions.InstanceStatus) { s.BackgroundCountersTrusted = true },
	} {
		status := base
		change(&status)
		if functionIdleEligible(status, 30, now) {
			t.Fatal("untrusted or non-idle observation authorized retirement")
		}
	}
	for _, timeout := range []int{-1, 0, 29, 3601} {
		if functionIdleEligible(base, timeout, now) {
			t.Fatal("invalid idle policy authorized retirement")
		}
	}
}
