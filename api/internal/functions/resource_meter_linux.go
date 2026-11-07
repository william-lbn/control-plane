//go:build linux

package functions

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func parseResourceCounter(data []byte) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || v < 0 {
		return 0, errors.New("invalid cgroup resource counter")
	}
	return v, nil
}

func (g *ChildGroup) ReadResourceMeter() (ResourceMeter, error) {
	if g == nil || g.path != "/sys/fs/cgroup/neon-function" || os.Geteuid() != 0 {
		return ResourceMeter{}, errors.New("privileged original child group required")
	}
	return readResourceMeterFiles(g.path)
}

func readResourceMeterFiles(path string) (ResourceMeter, error) {
	var out ResourceMeter
	cpu, err := os.ReadFile(filepath.Join(path, "cpu.stat"))
	if err != nil || len(cpu) > 4096 {
		return out, errors.New("cgroup CPU observation unavailable")
	}
	found := false
	for _, line := range strings.Split(string(cpu), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			if found {
				return out, errors.New("duplicate cgroup CPU counter")
			}
			out.CPUUsec, err = parseResourceCounter([]byte(fields[1]))
			found = true
			if err != nil {
				return out, err
			}
		}
	}
	if !found {
		return out, errors.New("cgroup CPU counter unavailable")
	}
	current, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return out, errors.New("cgroup memory observation unavailable")
	}
	out.MemoryBytes, err = parseResourceCounter(current)
	if err != nil {
		return out, err
	}
	peak, err := os.ReadFile(filepath.Join(path, "memory.peak"))
	if err != nil {
		return out, errors.New("cgroup peak memory observation unavailable")
	}
	out.MemoryPeakBytes, err = parseResourceCounter(peak)
	if err != nil || out.MemoryPeakBytes < out.MemoryBytes {
		return ResourceMeter{}, errors.New("invalid cgroup peak memory counter")
	}
	return out, nil
}
