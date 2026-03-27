package main

import (
	"errors"
	"fmt"
	"os"
)

func removePathIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

func closePIDFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("close PID file: %w", err)
	}

	return nil
}
