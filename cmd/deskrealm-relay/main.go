package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"deskrealm/internal/m3/install"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := install.RunRelay(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "deskrealm-relay:", err)
		os.Exit(1)
	}
}
