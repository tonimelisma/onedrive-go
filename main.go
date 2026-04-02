package main

import (
	"os"

	"github.com/tonimelisma/onedrive-go/internal/cli"
)

func run(args []string) int {
	return cli.Main(args)
}

func main() {
	os.Exit(run(os.Args[1:]))
}
