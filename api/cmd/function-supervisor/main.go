//go:build linux

// function-supervisor runs only inside a dedicated, immutable Functions guest.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := functions.ServeGuest(ctx); err != nil {
		// Bootstrap may contain customer secrets: underlying errors, URLs and
		// environment values must never flow into the guest/platform journal.
		stage := "unknown"
		var boot *functions.GuestBootError
		if errors.As(err, &boot) {
			stage = boot.Stage
		}
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"component": "function-supervisor", "state": "boot_or_runtime_failed", "stage": stage})
		os.Exit(1)
	}
	fmt.Println(`{"component":"function-supervisor","state":"stopped"}`)
}
