// Command tclaude operates the agentic work backend through its authenticated API.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tofutools/tclaude/internal/product"
)

func main() {
	if handled, err := product.TrySandboxChild(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	command := product.ClientCommand()
	command.Version = version
	if command.Version == "" {
		command.Version = "development"
	}
	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Stamped by release builds with -ldflags "-X main.version=...".
var version string
