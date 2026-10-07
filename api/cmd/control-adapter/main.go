package main

import (
	"context"
	"fmt"
	"github.com/william-lbn/control-plane/api/internal/control"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := control.RunProxyAdapter(ctx); err != nil {
		// Configuration and dependency errors may contain private details. Log type only.
		fmt.Fprintf(os.Stderr, "Proxy adapter stopped: %T\n", err)
		os.Exit(1)
	}
}
