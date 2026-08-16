package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aide-tools/swarrow/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.NewRootCommand(version).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
