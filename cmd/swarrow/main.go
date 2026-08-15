package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aide-tools/swarrow/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.NewRootCommand(version).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
