package devtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type StateAuditOptions struct {
	DBPath     string
	JSON       bool
	RepairSafe bool
	Stdout     io.Writer
}

type stateAuditOutput struct {
	Clean          bool                          `json:"clean"`
	RepairsApplied int                           `json:"repairs_applied,omitempty"`
	Findings       []syncengine.IntegrityFinding `json:"findings,omitempty"`
}

func RunStateAudit(ctx context.Context, opts StateAuditOptions) (retErr error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	if opts.DBPath == "" {
		return fmt.Errorf("state audit: missing db path")
	}
	if _, err := localpath.Stat(opts.DBPath); err != nil {
		return fmt.Errorf("state audit: stat db %s: %w", opts.DBPath, err)
	}

	logger := slog.New(slog.DiscardHandler)
	store, err := syncengine.NewSyncStore(ctx, opts.DBPath, logger)
	if err != nil {
		return fmt.Errorf("state audit: open sync store %s: %w", opts.DBPath, err)
	}
	closeCtx := context.WithoutCancel(ctx)
	defer func() {
		if closeErr := store.Close(closeCtx); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("state audit: close sync store: %w", closeErr)
		}
	}()

	repairsApplied := 0
	if opts.RepairSafe {
		repairsApplied, err = store.RepairIntegritySafe(ctx)
		if err != nil {
			return fmt.Errorf("state audit: repair integrity: %w", err)
		}
	}

	report, err := store.AuditIntegrity(ctx)
	if err != nil {
		return fmt.Errorf("state audit: audit integrity: %w", err)
	}

	output := stateAuditOutput{
		Clean:          !report.HasFindings(),
		RepairsApplied: repairsApplied,
		Findings:       report.Findings,
	}

	if opts.JSON {
		if err := writeStateAuditJSON(stdout, output); err != nil {
			return err
		}
	} else {
		if err := writeStateAuditText(stdout, output); err != nil {
			return err
		}
	}

	if report.HasFindings() {
		return fmt.Errorf("state audit found %d finding(s)", len(report.Findings))
	}

	return nil
}

func writeStateAuditJSON(w io.Writer, output stateAuditOutput) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("state audit: write json: %w", err)
	}

	return nil
}

func writeStateAuditText(w io.Writer, output stateAuditOutput) error {
	if output.Clean {
		if output.RepairsApplied > 0 {
			_, err := fmt.Fprintf(w, "state audit: clean after %d repair(s)\n", output.RepairsApplied)
			if err != nil {
				return fmt.Errorf("state audit: write text: %w", err)
			}
			return nil
		}

		if _, err := io.WriteString(w, "state audit: clean\n"); err != nil {
			return fmt.Errorf("state audit: write text: %w", err)
		}
		return nil
	}

	if output.RepairsApplied > 0 {
		if _, err := fmt.Fprintf(
			w,
			"state audit: %d finding(s) remain after %d repair(s)\n",
			len(output.Findings),
			output.RepairsApplied,
		); err != nil {
			return fmt.Errorf("state audit: write text: %w", err)
		}
	} else {
		if _, err := fmt.Fprintf(w, "state audit: %d finding(s)\n", len(output.Findings)); err != nil {
			return fmt.Errorf("state audit: write text: %w", err)
		}
	}

	for _, finding := range output.Findings {
		if _, err := fmt.Fprintf(w, "- %s: %s\n", finding.Code, finding.Detail); err != nil {
			return fmt.Errorf("state audit: write finding: %w", err)
		}
	}

	return nil
}
