module github.com/tonimelisma/onedrive-go

go 1.25.0

// The go directive is the language floor (os.Root gained its mutating methods
// in 1.25). The toolchain directive is separate and must track a patched
// release: CI resolves go-version-file from this file, and pinning the floor
// alone built against the unpatched 1.25.0 standard library, which govulncheck
// correctly rejected with 29 stdlib advisories.
toolchain go1.26.6

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/coder/websocket v1.8.15
	github.com/fsnotify/fsnotify v1.10.1
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.12.1
	golang.org/x/oauth2 v0.30.0
	golang.org/x/sync v0.22.0
	golang.org/x/text v0.41.0
	modernc.org/sqlite v1.57.0
)

require golang.org/x/sys v0.47.0

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Fork adds Config.OnTokenChange callback for token persistence on refresh.
// Tracks upstream golang/oauth2. Replace until upstream proposal golang/go#77502 lands.
replace golang.org/x/oauth2 => github.com/tonimelisma/oauth2 v0.0.0-20260209071456-249baba404ab
