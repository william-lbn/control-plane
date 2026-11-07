package main

import (
	"context"
	"github.com/william-lbn/control-plane/api/internal/control"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := control.RunStorageGateway(ctx); err != nil {
		slog.Error("Storage gateway stopped", "error", err)
		os.Exit(1)
	}
}
