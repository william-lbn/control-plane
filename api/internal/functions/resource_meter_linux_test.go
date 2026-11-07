//go:build linux

package functions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupResourcesAreActualCountersAndFailClosed(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.stat", "usage_usec 123456\nuser_usec 123000\nsystem_usec 456\n")
	write("memory.current", "65536\n")
	write("memory.peak", "131072\n")
	v, err := readResourceMeterFiles(dir)
	if err != nil || v.CPUUsec != 123456 || v.MemoryBytes != 65536 || v.MemoryPeakBytes != 131072 {
		t.Fatal("actual cgroup counters rejected", err)
	}
	for _, bad := range []string{"user_usec 4\n", "usage_usec -1\n", "usage_usec 1\nusage_usec 2\n", "usage_usec 9223372036854775808\n"} {
		write("cpu.stat", bad)
		if _, err := readResourceMeterFiles(dir); err == nil {
			t.Fatal("missing/corrupt cgroup CPU accepted")
		}
	}
	write("cpu.stat", "usage_usec 100\n")
	write("memory.peak", "2\n")
	if _, err := readResourceMeterFiles(dir); err == nil {
		t.Fatal("inconsistent memory observation accepted")
	}
	if err := os.Remove(filepath.Join(dir, "memory.peak")); err != nil {
		t.Fatal(err)
	}
	if _, err := readResourceMeterFiles(dir); err == nil {
		t.Fatal("missing memory was represented as zero usage")
	}
}
