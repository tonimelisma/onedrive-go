module github.com/tonimelisma/onedrive-go

go 1.24.0

toolchain go1.24.4

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.10.0
	golang.org/x/oauth2 v0.30.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Fork adds Config.OnTokenChange callback for token persistence on refresh.
// Tracks upstream golang/oauth2. Replace until upstream proposal golang/go#77502 lands.
replace golang.org/x/oauth2 => github.com/tonimelisma/oauth2 v0.0.0-20260209071456-249baba404ab
