package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	stdsynctest "testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// A one-shot model run runs in virtual time: inside a testing/synctest
// bubble, where the clock moves only when every goroutine of the run is
// durably blocked, and then straight to the next instant one of them waits
// for. A storm's 30-second Retry-After, a retry backoff and a ramp's
// admission instant cost no wall time, and which goroutine runs next no
// longer depends on how long the host took to get there. Every goroutine the
// run starts is in the bubble: the engine's, the store's (modernc SQLite is
// Go code over file syscalls, and a goroutine in a syscall only holds the
// clock until it returns), the fake OneDrive's and the chaos layer's, which
// answer in process. A watch run cannot join: its local observer waits on
// the kernel's file events, which do not block durably, so the clock would
// never move.
//
// The bubble also says what a hang is. A run whose goroutines all block for
// good is a deadlock the bubble reports at once (recoverDeadlock), with the
// fault summary, the saved fault log and the seed's replay line. A run that
// hangs another way (a goroutine spinning, or blocked on a lock) runs into go
// test's deadline, which runs no cleanup, so a wall-clock watchdog prints the
// same report shortly before the deadline (armHangReport).

// modelHangReportMargin is how long before go test's deadline a run still
// going prints its hang report.
const modelHangReportMargin = 30 * time.Second

// modelSeedReport is what a model seed reports when it ends, on the seed's
// own test outside any bubble: the chaos layer's fault summary always, and,
// when the seed failed or hung, the fault log saved to a file the report
// names.
type modelSeedReport struct {
	t    *testing.T
	seed uint64

	// mu guards chaos, which the run sets from inside its bubble.
	mu    stdsync.Mutex
	chaos *chaosGraph
	// reported is set once the report has been written, by the cleanup or
	// by the hang watchdog, whichever comes first.
	reported bool
}

// newModelSeedReport registers the seed's report on t. The caller registers
// the seed's replay line first (LogSeedReplayOnFailure), so the replay line,
// which runs last, ends what the seed prints.
func newModelSeedReport(t *testing.T, seed uint64) *modelSeedReport {
	t.Helper()

	r := &modelSeedReport{t: t, seed: seed}
	r.armHangReport()
	t.Cleanup(func() { r.write(r.t.Logf, r.t.Failed()) })

	return r
}

func (r *modelSeedReport) setChaos(chaos *chaosGraph) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chaos = chaos
}

// write writes the report through logf, once: the fault summary, and when
// failed, where the fault log was saved.
func (r *modelSeedReport) write(logf func(format string, args ...any), failed bool) {
	r.mu.Lock()
	if r.reported {
		r.mu.Unlock()
		return
	}
	r.reported = true
	chaos := r.chaos
	r.mu.Unlock()
	if chaos == nil {
		return
	}
	logf("seed %d chaos faults: %s", r.seed, chaos.faultSummary())
	if !failed {
		return
	}
	path, err := r.save("faults", chaosFaultLogText(chaos.drawLog()))
	if err != nil {
		logf("seed %d: the fault log could not be saved: %v", r.seed, err)
		return
	}
	logf("seed %d fault log: %s", r.seed, path)
}

// chaosFaultLogText is the fault log, one entry a line, in the order the
// entries happened: the stream, then the entry within it.
func chaosFaultLogText(log []chaosDraw) string {
	var b strings.Builder
	for _, draw := range log {
		b.WriteString(draw.stream() + " " + draw.String() + "\n")
	}

	return b.String()
}

// save writes content to a file of its own that outlives the test, so a
// failed seed leaves its evidence behind, and returns the file's path.
func (r *modelSeedReport) save(kind, content string) (string, error) {
	dir, err := os.MkdirTemp("", "onedrive-go-model-seed-")
	if err != nil {
		return "", fmt.Errorf("make the report directory: %w", err)
	}
	path := filepath.Join(dir, "seed-"+strconv.FormatUint(r.seed, 10)+"-"+kind+".log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write the %s: %w", kind, err)
	}

	return path, nil
}

// armHangReport prints the report and the seed's replay line to stderr
// shortly before go test's deadline, if the seed is still running then:
// the deadline ends the binary without running a cleanup, and output a test
// buffered is lost with it.
func (r *modelSeedReport) armHangReport() {
	deadline, ok := r.t.Deadline()
	if !ok {
		return
	}
	stderr := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	watchdog := time.AfterFunc(time.Until(deadline)-modelHangReportMargin, func() {
		stderr("%s: seed %d is still running %s before go test's deadline", r.t.Name(), r.seed, modelHangReportMargin)
		r.write(stderr, true)
		test, _, _ := strings.Cut(r.t.Name(), "/")
		stderr("%s", syncfixture.SeedReplayLine(test, "./internal/sync/", syncfixture.RaceDetectorEnabled,
			[]syncfixture.SeedAssignment{{Name: modelSeedEnv, Value: strconv.FormatUint(r.seed, 10)}}))
	})
	r.t.Cleanup(func() { watchdog.Stop() })
}

// runModelSeedInVirtualTime runs one seed inside a bubble of its own.
func runModelSeedInVirtualTime(t *testing.T, seed uint64, opts modelOptions, report *modelSeedReport) *modelRun {
	t.Helper()

	var run *modelRun
	func() {
		defer report.recoverDeadlock()
		stdsynctest.Test(t, func(t *testing.T) {
			// The bubble's clock stands still unless every goroutine of the
			// run waits, so how far it moved is how long the run waited.
			bubbleStart := time.Now()
			run = runModelSeed(t, seed, opts, report)
			run.waited = time.Since(bubbleStart)
		})
	}()

	return run
}

// recoverDeadlock turns the bubble's deadlock, every goroutine of the run
// blocked for good, into the seed's failure, with a dump of every goroutine
// saved beside the fault log; the seed's cleanups then write its report and
// its replay line. Any other panic goes on.
func (r *modelSeedReport) recoverDeadlock() {
	recovered := recover()
	if recovered == nil {
		return
	}
	if !strings.HasPrefix(fmt.Sprint(recovered), "deadlock") {
		panic(recovered)
	}
	where := "not saved"
	if path, err := r.save("goroutines", allGoroutines()); err == nil {
		where = path
	}
	require.Failf(r.t, "the model run deadlocked", "seed %d: %v; every goroutine: %s", r.seed, recovered, where)
}

// allGoroutines is the stack of every goroutine of the test binary.
func allGoroutines() string {
	const maxDump = 64 << 20
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= maxDump {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// Validates: R-6.10.33
//
// A chaos seed waits out its storm's Retry-After answers, backoffs and
// admission instants in virtual time: the run waits longer on its clock than
// it takes on the wall clock. Before the bubble every wait ran for real (71 s
// for seed 1 and 119 s for seed 2 on a 4-core host), so the run took at
// least as long as it waited.
func TestModel_ChaosRunWaitsInVirtualTime(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	const seed = chaosRaceSeed
	started := time.Now()
	run := runModelScenarioWith(t, seed, modelOptions{chaos: true, batches: 1})
	took := time.Since(started)

	t.Logf("seed %d waited %s and took %s", seed, run.waited, took)
	require.Positive(t, run.chaos.faultCount(), "the storm injected no fault")
	assert.Less(t, took, run.waited, "the run took as long as it waited: its waits ran on the wall clock")
}
