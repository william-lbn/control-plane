package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/william-lbn/control-plane/api/internal/functionedge"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := functionedge.Run(ctx); err != nil {
		slog.Error("function edge failed", "error", err)
		os.Exit(1)
	}
}
