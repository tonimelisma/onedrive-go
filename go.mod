module github.com/tonimelisma/onedrive-go

go 1.24.0

toolchain go1.24.4

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.10.0
	golang.org/x/oauth2 v0.30.0
	golang.org/x/sync v0.19.0
	golang.org/x/sys v0.37.0
	golang.org/x/text v0.34.0
	golang.org/x/time v0.14.0
	modernc.org/sqlite v1.46.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Fork adds Config.OnTokenChange callback for token persistence on refresh.
// Tracks upstream golang/oauth2. Replace until upstream proposal golang/go#77502 lands.
replace golang.org/x/oauth2 => github.com/tonimelisma/oauth2 v0.0.0-20260209071456-249baba404ab
