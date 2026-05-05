package perf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	captureDirPerm  = 0o700
	captureFilePerm = 0o600
)

var (
	ErrCaptureInProgress  = errors.New("performance capture already in progress")
	ErrCaptureUnavailable = errors.New("performance capture is unavailable")
)

type CaptureOptions struct {
	Duration   time.Duration
	OutputDir  string
	Trace      bool
	FullDetail bool
	OwnerMode  string
}

type CaptureResult struct {
	OutputDir     string `json:"output_dir"`
	ManifestPath  string `json:"manifest_path"`
	CPUProfile    string `json:"cpu_profile"`
	HeapProfile   string `json:"heap_profile"`
	BlockProfile  string `json:"block_profile"`
	MutexProfile  string `json:"mutex_profile"`
	GoroutineDump string `json:"goroutine_dump"`
	TracePath     string `json:"trace_path,omitempty"`
}

type captureManifest struct {
	OwnerMode         string              `json:"owner_mode"`
	StartedAt         time.Time           `json:"started_at"`
	CompletedAt       time.Time           `json:"completed_at"`
	DurationMS        int64               `json:"duration_ms"`
	ManagedDriveCount int                 `json:"managed_drive_count"`
	Aggregate         Snapshot            `json:"aggregate"`
	DriveSnapshots    map[string]Snapshot `json:"drive_snapshots,omitempty"`
}

func captureBundle(
	ctx context.Context,
	opts CaptureOptions,
	aggregate *Snapshot,
	driveSnapshots map[string]Snapshot,
) (CaptureResult, error) {
	outputDir, err := prepareCaptureDir(opts.OutputDir)
	if err != nil {
		return CaptureResult{}, err
	}

	startedAt := time.Now()
	result := CaptureResult{
		OutputDir:     outputDir,
		ManifestPath:  filepath.Join(outputDir, "manifest.json"),
		CPUProfile:    filepath.Join(outputDir, "cpu.pprof"),
		HeapProfile:   filepath.Join(outputDir, "heap.pprof"),
		BlockProfile:  filepath.Join(outputDir, "block.pprof"),
		MutexProfile:  filepath.Join(outputDir, "mutex.pprof"),
		GoroutineDump: filepath.Join(outputDir, "goroutine.txt"),
	}
	if opts.Trace {
		result.TracePath = filepath.Join(outputDir, "trace.out")
	}

	captureErr := runCapture(ctx, opts, &result)
	if captureErr != nil {
		return CaptureResult{}, captureErr
	}

	completedAt := time.Now()
	manifest := captureManifest{
		OwnerMode:         opts.OwnerMode,
		StartedAt:         startedAt,
		CompletedAt:       completedAt,
		DurationMS:        durationMS(completedAt.Sub(startedAt)),
		ManagedDriveCount: len(driveSnapshots),
	}
	if aggregate != nil {
		manifest.Aggregate = *aggregate
	}
	if opts.FullDetail {
		manifest.DriveSnapshots = driveSnapshots
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return CaptureResult{}, fmt.Errorf("encode capture manifest: %w", err)
	}
	if err := localpath.WriteDisposableFile(result.ManifestPath, data, captureFilePerm); err != nil {
		return CaptureResult{}, fmt.Errorf("write capture manifest: %w", err)
	}

	return result, nil
}

func prepareCaptureDir(outputDir string) (string, error) {
	if outputDir != "" {
		if err := localpath.MkdirAll(outputDir, captureDirPerm); err != nil {
			return "", fmt.Errorf("create capture directory: %w", err)
		}
		return outputDir, nil
	}

	tempRoot, err := localpath.MkdirTemp(os.TempDir(), "onedrive-go-perf-*")
	if err != nil {
		return "", fmt.Errorf("create temp capture directory: %w", err)
	}

	return tempRoot, nil
}

func runCapture(ctx context.Context, opts CaptureOptions, result *CaptureResult) (err error) {
	cpuFile, err := localpath.OpenFile(result.CPUProfile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, captureFilePerm)
	if err != nil {
		return fmt.Errorf("open CPU profile: %w", err)
	}
	defer func() {
		err = joinError(err, closeCaptureFile(cpuFile, "close CPU profile"))
	}()

	startCPUProfileErr := pprof.StartCPUProfile(cpuFile)
	if startCPUProfileErr != nil {
		return fmt.Errorf("start CPU profile: %w", startCPUProfileErr)
	}
	defer pprof.StopCPUProfile()

	var traceFile *os.File
	if opts.Trace {
		traceFile, err = localpath.OpenFile(result.TracePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, captureFilePerm)
		if err != nil {
			return fmt.Errorf("open trace output: %w", err)
		}
		defer func() {
			err = joinError(err, closeCaptureFile(traceFile, "close trace output"))
		}()

		startTraceErr := trace.Start(traceFile)
		if startTraceErr != nil {
			return fmt.Errorf("start trace: %w", startTraceErr)
		}
		defer trace.Stop()
	}

	previousMutexRate := runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	defer func() {
		runtime.SetMutexProfileFraction(previousMutexRate)
		runtime.SetBlockProfileRate(0)
	}()

	timer := time.NewTimer(opts.Duration)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
		return fmt.Errorf("capture canceled: %w", ctx.Err())
	}

	if err := writeHeapProfile(result.HeapProfile); err != nil {
		return err
	}
	if err := writeLookupProfile("block", result.BlockProfile, 0); err != nil {
		return err
	}
	if err := writeLookupProfile("mutex", result.MutexProfile, 0); err != nil {
		return err
	}
	if err := writeLookupProfile("goroutine", result.GoroutineDump, 2); err != nil {
		return err
	}

	return nil
}

func writeHeapProfile(path string) (err error) {
	file, err := localpath.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, captureFilePerm)
	if err != nil {
		return fmt.Errorf("open heap profile: %w", err)
	}
	defer func() {
		err = joinError(err, closeCaptureFile(file, "close heap profile"))
	}()

	runtime.GC()
	if err := pprof.WriteHeapProfile(file); err != nil {
		return fmt.Errorf("write heap profile: %w", err)
	}

	return nil
}

func writeLookupProfile(name, path string, debug int) (err error) {
	file, err := localpath.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, captureFilePerm)
	if err != nil {
		return fmt.Errorf("open %s profile: %w", name, err)
	}
	defer func() {
		err = joinError(err, closeCaptureFile(file, fmt.Sprintf("close %s profile", name)))
	}()

	profile := pprof.Lookup(name)
	if profile == nil {
		return fmt.Errorf("lookup %s profile: unavailable", name)
	}
	if err := profile.WriteTo(file, debug); err != nil {
		return fmt.Errorf("write %s profile: %w", name, err)
	}

	return nil
}

func closeCaptureFile(file *os.File, action string) error {
	if file == nil {
		return nil
	}

	closeErr := file.Close()
	if closeErr == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", action, closeErr)
}

func joinError(current error, next error) error {
	if next == nil {
		return current
	}
	if current == nil {
		return next
	}

	return errors.Join(current, next)
}
