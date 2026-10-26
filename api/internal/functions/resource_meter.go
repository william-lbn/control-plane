package functions

import (
	"encoding/json"
	"net/http"
	"time"
)

type ResourceMeter struct {
	Scope           Scope     `json:"scope"`
	BootID          string    `json:"boot_id"`
	CPUUsec         int64     `json:"cpu_usec"`
	MemoryBytes     int64     `json:"memory_bytes"`
	MemoryPeakBytes int64     `json:"memory_peak_bytes"`
	ObservedAt      time.Time `json:"observed_at"`
	Source          string    `json:"source"`
}

func (m *Manager) resources(w http.ResponseWriter, r *http.Request) {
	if m.resourceMeter == nil {
		managerError(w, 503, "resource_meter_unavailable")
		return
	}
	v, err := m.resourceMeter()
	if err != nil {
		managerError(w, 503, "resource_meter_unavailable")
		return
	}
	v.Scope, v.BootID, v.ObservedAt, v.Source = m.scope, m.bootID, time.Now().UTC(), "supervisor_cgroup_v2"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
