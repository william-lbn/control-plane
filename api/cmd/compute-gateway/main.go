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
	if err := control.RunComputeGateway(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Compute gateway stopped:", fmt.Sprintf("%T", err))
		os.Exit(1)
	}
}
