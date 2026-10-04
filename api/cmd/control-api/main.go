package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/william-lbn/control-plane/api/internal/control"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := control.Run(ctx); err != nil {
		slog.Error("control service failed", "error", err)
		os.Exit(1)
	}
}
