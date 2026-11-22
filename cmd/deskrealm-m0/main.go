package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"deskrealm/internal/m0"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := m0.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "deskrealm-m0:", err)
		os.Exit(1)
	}
}
