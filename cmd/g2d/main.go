package main

import (
	"fmt"
	"os"

	"github.com/local/google-to-domate/internal/config"
	"github.com/local/google-to-domate/internal/ui"
)

func main() {
	settings, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}
	if err := ui.Run(settings); err != nil {
		fmt.Fprintf(os.Stderr, "gui failed: %v\n", err)
		os.Exit(1)
	}
}
