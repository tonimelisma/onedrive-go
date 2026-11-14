package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

const (
	perfCaptureDefaultDuration = 15 * time.Second
	perfCaptureMinDuration     = 5 * time.Second
	perfCaptureMaxDuration     = 60 * time.Second
)

// perfCaptureUnavailableMessage is the error for a sync owner the capture
// cannot reach: one that did not answer the probe, or a state the probe does
// not know.
const perfCaptureUnavailableMessage = "live perf capture unavailable; check logs"

func newPerfCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "perf",
		Short:       "Inspect and capture live sync performance data",
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
	}

	cmd.AddCommand(newPerfCaptureCmd())

	return cmd
}

func newPerfCaptureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "capture",
		Short:       "Capture CPU, heap, mutex, block, and goroutine profiles from the active sync owner",
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runPerfCapture,
	}

	cmd.Flags().Duration("duration", perfCaptureDefaultDuration, "capture duration")
	cmd.Flags().String("output", "", "output directory for the capture bundle")
	cmd.Flags().Bool("trace", false, "include a Go execution trace in the bundle")
	cmd.Flags().Bool("full-detail", false, "include per-mount live perf details in the capture manifest")

	return cmd
}

func runPerfCapture(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())

	duration, err := cmd.Flags().GetDuration("duration")
	if err != nil {
		return fmt.Errorf("read --duration flag: %w", err)
	}
	if duration < perfCaptureMinDuration || duration > perfCaptureMaxDuration {
		return fmt.Errorf(
			"--duration must be between %s and %s",
			perfCaptureMinDuration,
			perfCaptureMaxDuration,
		)
	}

	outputDir, err := cmd.Flags().GetString("output")
	if err != nil {
		return fmt.Errorf("read --output flag: %w", err)
	}
	includeTrace, err := cmd.Flags().GetBool("trace")
	if err != nil {
		return fmt.Errorf("read --trace flag: %w", err)
	}
	fullDetail, err := cmd.Flags().GetBool("full-detail")
	if err != nil {
		return fmt.Errorf("read --full-detail flag: %w", err)
	}

	probe, err := probeControlOwner(cmd.Context())
	if err != nil {
		return fmt.Errorf("probe active sync owner: %w", err)
	}
	switch probe.state {
	case controlOwnerStateNoSocket:
		return errors.New("no active sync owner; check logs for historical performance summaries")
	case controlOwnerStatePathUnavailable, controlOwnerStateProbeFailed, controlOwnerStateUnresponsive:
		return errors.New(perfCaptureUnavailableMessage)
	case controlOwnerStateWatchOwner, controlOwnerStateOneShotOwner:
	default:
		return errors.New(perfCaptureUnavailableMessage)
	}

	response, err := probe.client.capturePerf(cmd.Context(), synccontrol.PerfCaptureRequest{
		DurationMS: duration.Milliseconds(),
		OutputDir:  outputDir,
		Trace:      includeTrace,
		FullDetail: fullDetail,
	})
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), &response)
	}

	return printPerfCaptureText(cc.Output(), &response)
}

func printPerfCaptureText(w io.Writer, response *synccontrol.PerfCaptureResponse) error {
	result := response.Result
	if err := writef(w, "Capture bundle: %s\n", result.OutputDir); err != nil {
		return err
	}
	if err := writef(w, "Manifest: %s\n", result.ManifestPath); err != nil {
		return err
	}
	if err := writef(w, "CPU profile: %s\n", result.CPUProfile); err != nil {
		return err
	}
	if err := writef(w, "Heap profile: %s\n", result.HeapProfile); err != nil {
		return err
	}
	if err := writef(w, "Block profile: %s\n", result.BlockProfile); err != nil {
		return err
	}
	if err := writef(w, "Mutex profile: %s\n", result.MutexProfile); err != nil {
		return err
	}
	if err := writef(w, "Goroutines: %s\n", result.GoroutineDump); err != nil {
		return err
	}
	if result.TracePath != "" {
		if err := writef(w, "Trace: %s\n", result.TracePath); err != nil {
			return err
		}
	}

	return nil
}
