package main

import (
	"fmt"
	"os"

	"github.com/intrusiveai-engineering/releases/internal/catalogcli"
)

func main() {
	if err := catalogcli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
