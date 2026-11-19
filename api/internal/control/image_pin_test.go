package control

import (
	"regexp"
	"testing"
)

func TestPinnedDigestAllowsHarborHostPort(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9][a-z0-9.:/_-]*@sha256:[a-f0-9]{64}$`)
	ok := []string{
		"docker.io/williamluckyli/vm-compute-node-v16@sha256:4716bf4c0d5437b05977787398b764b5638ab42bbc657dcc6909fd39c095744d",
		"10.11.9.236:5000/neon/williamluckyli-vm-compute-node-v16@sha256:4716bf4c0d5437b05977787398b764b5638ab42bbc657dcc6909fd39c095744d",
	}
	bad := []string{"repo:tag", "10.11.9.236:5000/neon/x:latest", ""}
	for _, v := range ok {
		if !re.MatchString(v) {
			t.Fatalf("expected pin to match: %s", v)
		}
	}
	for _, v := range bad {
		if re.MatchString(v) {
			t.Fatalf("expected pin to reject: %s", v)
		}
	}
}
