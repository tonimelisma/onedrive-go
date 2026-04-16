package devtool

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const benchSharedFixtureEnvPath = ".testdata/fixtures.env"

type benchLiveConfig struct {
	PrimaryDrive string
}

func loadBenchLiveConfig(repoRoot string) (benchLiveConfig, error) {
	if err := loadBenchDotEnv(filepath.Join(repoRoot, ".env")); err != nil {
		return benchLiveConfig{}, err
	}
	if err := loadBenchDotEnv(filepath.Join(repoRoot, benchSharedFixtureEnvPath)); err != nil {
		return benchLiveConfig{}, err
	}

	primaryDrive := os.Getenv("ONEDRIVE_TEST_DRIVE")
	if primaryDrive == "" {
		return benchLiveConfig{}, fmt.Errorf("load live benchmark config: ONEDRIVE_TEST_DRIVE not set")
	}

	return benchLiveConfig{
		PrimaryDrive: primaryDrive,
	}, nil
}

func loadBenchDotEnv(path string) (retErr error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("close %s: %w", path, closeErr)
		}
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set env %s from %s: %w", key, path, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}

	return nil
}
