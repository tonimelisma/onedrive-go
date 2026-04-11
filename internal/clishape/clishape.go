// Package clishape owns the pure-data contract for the product CLI grammar.
package clishape

// FlagSpec describes one documented CLI flag and whether it consumes the next
// token as a value.
type FlagSpec struct {
	Name          string
	ConsumesValue bool
}

// CommandSpec is the pure-data contract for one product CLI command.
type CommandSpec struct {
	Name        string
	Runnable    bool
	Flags       []FlagSpec
	Subcommands []CommandSpec
}

// Root returns the current product CLI command tree. The verifier and CLI
// command-contract tests share this single source of truth so the documented
// grammar cannot drift between packages.
func Root() CommandSpec {
	return CommandSpec{
		Name:     "onedrive-go",
		Flags:    rootFlags(),
		Runnable: false,
		Subcommands: []CommandSpec{
			command("login", boolFlag("browser")),
			command("logout", boolFlag("purge")),
			command("whoami"),
			command("status", boolFlag("history"), boolFlag("perf")),
			command("shared"),
			command("ls"),
			command("get"),
			command("put"),
			command("rm", boolFlag("recursive"), boolFlag("permanent")),
			command("mkdir"),
			command("stat"),
			command("pause"),
			command("resume"),
			command("recover", boolFlag("yes")),
			command("mv", boolFlag("force")),
			command("cp", boolFlag("force")),
			{
				Name:     "perf",
				Runnable: false,
				Subcommands: []CommandSpec{
					command("capture", valueFlag("duration"), valueFlag("output"), boolFlag("trace"), boolFlag("full-detail")),
				},
			},
			command(
				"sync",
				boolFlag("download-only"),
				boolFlag("upload-only"),
				boolFlag("dry-run"),
				boolFlag("watch"),
				boolFlag("full"),
			),
			{
				Name:     "drive",
				Runnable: false,
				Subcommands: []CommandSpec{
					command("list", boolFlag("all")),
					command("add"),
					command("remove", boolFlag("purge")),
					command("search"),
				},
			},
			{
				Name:     "resolve",
				Runnable: false,
				Subcommands: []CommandSpec{
					command("deletes"),
					command("local", boolFlag("all"), boolFlag("dry-run")),
					command("remote", boolFlag("all"), boolFlag("dry-run")),
					command("both", boolFlag("all"), boolFlag("dry-run")),
				},
			},
			{
				Name:     "recycle-bin",
				Runnable: false,
				Subcommands: []CommandSpec{
					command("list"),
					command("restore"),
					command("empty", boolFlag("confirm")),
				},
			},
		},
	}
}

func rootFlags() []FlagSpec {
	return []FlagSpec{
		valueFlag("config"),
		valueFlag("account"),
		valueFlag("drive"),
		boolFlag("json"),
		boolFlag("verbose"),
		boolFlag("debug"),
		boolFlag("quiet"),
	}
}

func command(name string, flags ...FlagSpec) CommandSpec {
	return CommandSpec{
		Name:     name,
		Runnable: true,
		Flags:    flags,
	}
}

func boolFlag(name string) FlagSpec {
	return FlagSpec{Name: name}
}

func valueFlag(name string) FlagSpec {
	return FlagSpec{
		Name:          name,
		ConsumesValue: true,
	}
}
