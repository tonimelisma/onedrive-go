package main

import (
	"errors"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if errors.Is(err, errVerifyMismatch) {
			os.Exit(1)
		}

		exitOnError(err)
	}
}
