package config

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHolder(t *testing.T) {
	cfg := DefaultConfig()
	h := NewHolder(cfg, "/etc/onedrive/config.toml")

	require.NotNil(t, h)
	assert.Equal(t, cfg, h.Config())
	assert.Equal(t, "/etc/onedrive/config.toml", h.Path())
}

func TestHolder_Update(t *testing.T) {
	cfg1 := DefaultConfig()
	h := NewHolder(cfg1, "/tmp/config.toml")

	cfg2 := DefaultConfig()
	cfg2.PollInterval = "10m"

	h.Update(cfg2)

	got := h.Config()
	assert.Equal(t, cfg2, got)
	assert.NotEqual(t, cfg1, got)
}

func TestHolder_PathImmutable(t *testing.T) {
	h := NewHolder(DefaultConfig(), "/original/path.toml")

	// Path is immutable — no setter. Multiple calls return the same value.
	assert.Equal(t, "/original/path.toml", h.Path())
	assert.Equal(t, "/original/path.toml", h.Path())
}

func TestHolder_ConcurrentReadWrite(t *testing.T) {
	cfg := DefaultConfig()
	h := NewHolder(cfg, "/tmp/config.toml")

	var wg sync.WaitGroup

	// 20 concurrent readers.
	for range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 100 {
				got := h.Config()
				assert.NotNil(t, got)
				_ = h.Path()
			}
		}()
	}

	// 5 concurrent writers.
	for range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 100 {
				h.Update(DefaultConfig())
			}
		}()
	}

	wg.Wait()
}
