package triggers

import (
	"testing"
	"time"
)

func TestScheduleOccurrenceIdentityStableAndScoped(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	branch, trigger := "br_0000000000000001", "ftr_000000000000000000000002"
	id, err := ScheduleInvocationID(branch, trigger, at)
	if err != nil || len(id) != 68 {
		t.Fatal("occurrence identity invalid", err)
	}
	same, err := ScheduleInvocationID(branch, trigger, at.In(time.FixedZone("local", 8*3600)))
	if err != nil || same != id {
		t.Fatal("time zone altered original occurrence")
	}
	for _, sample := range []struct {
		branch, trigger string
		at              time.Time
	}{{"br_0000000000000003", trigger, at}, {branch, "ftr_000000000000000000000004", at}, {branch, trigger, at.Add(time.Minute)}} {
		other, err := ScheduleInvocationID(sample.branch, sample.trigger, sample.at)
		if err != nil || other == id {
			t.Fatal("occurrence scope collision")
		}
	}
	if _, err = ScheduleInvocationID(branch, trigger, at.Add(time.Second)); err == nil {
		t.Fatal("non-minute occurrence admitted")
	}
}
