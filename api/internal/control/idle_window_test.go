package control

import (
	"testing"
	"time"
)

func TestIdleWindowUsesContinuousCurrentGenerationEvidence(t *testing.T) {
	now := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)
	zero, one := 0, 1
	at := func(age int) idleSample {
		return idleSample{At: now.Add(-time.Duration(age) * time.Second), State: "active", Connections: &zero}
	}
	valid := func() []idleSample { return []idleSample{at(75), at(45), at(15)} }
	type testCase struct {
		name    string
		samples []idleSample
		born    time.Time
		want    bool
	}
	tests := []testCase{
		{"continuous", valid(), now.Add(-time.Hour), true},
		{"new_vm_cannot_inherit_old_samples", valid(), now.Add(-35 * time.Second), false},
		{"no_birth_identity", valid(), time.Time{}, false},
		{"future_birth", valid(), now.Add(time.Second), false},
		{"stale", []idleSample{at(120), at(90), at(60)}, now.Add(-time.Hour), false},
		{"one_sample", []idleSample{at(15)}, now.Add(-time.Hour), false},
		{"too_short_no_jitter_discount", []idleSample{at(60), at(30)}, now.Add(-time.Hour), false},
		{"gap", []idleSample{at(120), at(90), at(15)}, now.Add(-time.Hour), false},
		{"future_sample", []idleSample{at(75), at(45), at(-1)}, now.Add(-time.Hour), false},
		{"unsorted", []idleSample{at(45), at(75), at(15)}, now.Add(-time.Hour), false},
		{"duplicate_time", []idleSample{at(75), at(75), at(15)}, now.Add(-time.Hour), false},
		{"old_generation_only", valid(), now.Add(-10 * time.Second), false},
	}
	for _, state := range []string{"suspended", "starting", "unknown"} {
		samples := valid()
		samples[1].State = state
		tests = append(tests, testCase{state, samples, now.Add(-time.Hour), false})
	}
	for _, reason := range []string{"null", "client", "postgres_error"} {
		samples := valid()
		switch reason {
		case "null":
			samples[1].Connections = nil
		case "client":
			samples[1].Connections = &one
		case "postgres_error":
			samples[1].PostgresError = true
		}
		tests = append(tests, testCase{reason, samples, now.Add(-time.Hour), false})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := idleWindowEligible(test.samples, now, test.born, time.Minute); got != test.want {
				t.Fatalf("eligible=%v want=%v", got, test.want)
			}
		})
	}
	reset := valid()
	reset[0].Connections = &one
	reset = append(reset, at(0))
	if idleWindowEligible(reset, now, now.Add(-time.Hour), 45*time.Second) {
		t.Fatal("unsupported sub-minute policy")
	}
	reset = []idleSample{at(150), at(120), at(90), at(60), at(30), at(0)}
	reset[1].Connections = &one
	if !idleWindowEligible(reset, now, now.Add(-time.Hour), time.Minute) {
		t.Fatal("valid new window after activity never recovers")
	}
}
