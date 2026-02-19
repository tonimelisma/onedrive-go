module github.com/tonimelisma/onedrive-go

go 1.24.0

toolchain go1.24.4

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/stretchr/testify v1.10.0
	golang.org/x/oauth2 v0.0.0-00010101000000-000000000000
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Fork adds Config.OnTokenChange callback for token persistence on refresh.
// Tracks upstream golang/oauth2. Replace until upstream proposal golang/go#77502 lands.
replace golang.org/x/oauth2 => github.com/tonimelisma/oauth2 v0.0.0-20260209071456-249baba404ab
