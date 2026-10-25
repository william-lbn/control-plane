package control

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Hard format bounds and operator admission budgets are distinct. Raising the
// format ceiling never raises an existing installation's admitted workload.
const storageObjectFormatLimit = 5 << 30
const storageBranchFormatLimit = 20 << 30

type storageCapacityConfig struct {
	ObjectBytes     int64
	BranchBytes     int64
	BufferBytes     int64
	TransferTimeout time.Duration
}

var storageCapacitySnapshot atomic.Pointer[storageCapacityConfig]
var storageCapacityDefaults = storageCapacityConfig{ObjectBytes: 64 << 20, BranchBytes: 256 << 20, BufferBytes: 256 << 20, TransferTimeout: 150 * time.Second}

func currentStorageCapacity() storageCapacityConfig {
	if value := storageCapacitySnapshot.Load(); value != nil {
		return *value
	}
	return storageCapacityDefaults
}

func parseStorageCapacity(getenv func(string) string) (storageCapacityConfig, error) {
	v := storageCapacityDefaults
	seconds := int64(v.TransferTimeout / time.Second)
	for _, field := range []struct {
		name     string
		target   *int64
		min, max int64
	}{
		{"NEON_STORAGE_OBJECT_BYTES", &v.ObjectBytes, storagePartLimit, storageObjectFormatLimit},
		{"NEON_STORAGE_BRANCH_BYTES", &v.BranchBytes, storagePartLimit, storageBranchFormatLimit},
		{"NEON_STORAGE_BUFFER_BYTES", &v.BufferBytes, 16 << 20, 12 << 30},
		{"NEON_STORAGE_TRANSFER_SECONDS", &seconds, 150, 3600},
	} {
		raw := getenv(field.name)
		if raw == "" {
			continue
		}
		if len(raw) > 20 || strings.Trim(raw, "0123456789") != "" {
			return v, errors.New("storage capacity must use bounded unsigned decimal integers")
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < field.min || value > field.max {
			return v, errors.New("storage capacity is outside the supported format budget")
		}
		*field.target = value
	}
	if v.BranchBytes < v.ObjectBytes || v.BufferBytes < 2*v.ObjectBytes {
		return v, errors.New("storage branch and two-slot private buffer budgets must cover the configured object size")
	}
	v.TransferTimeout = time.Duration(seconds) * time.Second
	return v, nil
}

// Startup only. Operators change the immutable Pod configuration and roll it;
// no request or tenant may mutate these process budgets.
func configureStorageCapacity() error {
	v, err := parseStorageCapacity(os.Getenv)
	if err != nil {
		return err
	}
	storageCapacitySnapshot.Store(&v)
	return nil
}

func storageObjectAdmissionLimit() int64 { return currentStorageCapacity().ObjectBytes }
func storageBranchAdmissionLimit() int64 { return currentStorageCapacity().BranchBytes }

// Override the server's ordinary JSON deadlines only for an admitted storage
// exchange. Keep a finite transfer budget, including authentication and cleanup.
func storageTransferDeadlines(w http.ResponseWriter) {
	deadline := time.Now().Add(currentStorageCapacity().TransferTimeout)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline.Add(5 * time.Second))
}
