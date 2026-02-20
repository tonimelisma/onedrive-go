package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	cmd.AddCommand(newConfigShowCmd())

	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Display effective configuration after all overrides",
		RunE:  runConfigShow,
	}
}

func runConfigShow(_ *cobra.Command, _ []string) error {
	if resolvedCfg == nil {
		return fmt.Errorf("no configuration loaded")
	}

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(resolvedCfg)
	}

	return config.RenderEffective(resolvedCfg, os.Stdout)
}
