package config

import "sync"

// Holder provides thread-safe access to a mutable *Config and an immutable
// config file path. Both SessionProvider and Orchestrator read through a
// shared Holder, so SIGHUP reload updates config in exactly one place.
type Holder struct {
	mu   sync.RWMutex
	cfg  *Config
	path string // immutable after construction
}

// NewHolder creates a Holder with the initial config and config file path.
func NewHolder(cfg *Config, path string) *Holder {
	return &Holder{
		cfg:  cfg,
		path: path,
	}
}

// Config returns the current config snapshot. Thread-safe (read lock).
func (h *Holder) Config() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.cfg
}

// Path returns the config file path. Thread-safe without locking because
// the path is immutable after construction.
func (h *Holder) Path() string {
	return h.path
}

// Update replaces the config. Thread-safe (write lock). Called on SIGHUP
// reload — one call updates config for all consumers (SessionProvider,
// Orchestrator).
func (h *Holder) Update(cfg *Config) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.cfg = cfg
}
