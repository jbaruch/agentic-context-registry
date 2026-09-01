package main

import (
	"fmt"
	"io"
	"os"
)

var version = "dev"

func main() {
	os.Exit(run(os.Stdout, os.Stderr, os.Args[1:]))
}

func run(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "acr is the Agentic Context Registry CLI")
		return 0
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, "usage: acr [help|version]")
		return 0
	default:
		fmt.Fprintf(stderr, "acr: unknown command %q\n", args[0])
		return 2
	}
}
