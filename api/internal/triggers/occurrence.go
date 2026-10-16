package triggers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var branchID = regexp.MustCompile(`^br_[a-f0-9]{16}$`)
var triggerID = regexp.MustCompile(`^ftr_[a-f0-9]{24}$`)

// ScheduleInvocationID is correlation/deduplication data, not a credential. Its
// identity remains stable through retries and changes across branches/runs.
func ScheduleInvocationID(branch, trigger string, at time.Time) (string, error) {
	if !branchID.MatchString(branch) || !triggerID.MatchString(trigger) || at.IsZero() || at.Second() != 0 || at.Nanosecond() != 0 {
		return "", errors.New("invalid schedule occurrence identity")
	}
	raw, _ := json.Marshal([]string{"function-schedule-v1", branch, trigger, at.UTC().Format(time.RFC3339)})
	sum := sha256.Sum256(raw)
	return "fti_" + hex.EncodeToString(sum[:]), nil
}
