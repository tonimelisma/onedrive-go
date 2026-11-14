package devtool

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSyncPackage is the import path go test reports for internal/sync.
const testSyncPackage = "github.com/tonimelisma/onedrive-go/internal/sync"

// Validates: R-6.10.23, R-6.10.26
//
// A GitHub Actions job log starts every line with a timestamp, and its first
// line with a byte-order mark. The nightly check reads job logs and `verify
// failed-test` reads downloaded ones, so a raw failure line must be found
// behind the prefix, and the package line that closes it must still give the
// failed test its package and the seed that package ran with.
func TestParseGoTestLog_TimestampedRawLines(t *testing.T) {
	t.Parallel()

	log := parseGoTestLog(strings.Join([]string{
		"\ufeff2026-11-11T10:20:00.1234567Z === RUN   TestWatchLoop",
		"2026-11-11T10:20:00.2234567Z -test.shuffle 1731340000000000000",
		"2026-11-11T10:21:00.1234567Z --- FAIL: TestWatchLoop (49.50s)",
		"2026-11-11T10:21:00.1234567Z     --- FAIL: TestWatchLoop/closed_scheduler (1.00s)",
		"2026-11-11T10:21:00.1234567Z FAIL",
		"2026-11-11T10:21:00.1234567Z FAIL\t" + testSyncPackage + "\t49.612s",
	}, "\n"))

	assert.Equal(t, []string{"TestWatchLoop"}, log.failedTopLevelTests())
	last, ok := log.lastFailure()
	require.True(t, ok)
	assert.Equal(t, goTestFailure{pkg: testSyncPackage, test: "TestWatchLoop/closed_scheduler"}, last)
	assert.Equal(t, "1731340000000000000", log.shuffleSeed(testSyncPackage))
	assert.False(t, log.deadline)
}

// Validates: R-6.10.23, R-6.10.26
//
// A raw line can hold a brace, such as a test's own JSON in its failure
// message. Only a line that is a go test -json event decodes as one: reading
// JSON from the first brace anywhere in a line reported tests that never ran.
// A real event behind a CI timestamp still decodes.
func TestParseGoTestLog_BraceInARawLineIsNotJSON(t *testing.T) {
	t.Parallel()

	raw := parseGoTestLog(strings.Join([]string{
		"2026-11-11T10:20:00Z --- FAIL: TestDecode (0.01s)",
		`2026-11-11T10:20:00Z     decode_test.go:12: got {"Action":"fail","Package":"example/bogus","Test":"TestBogus"}`,
		`    printed {"Action":"fail","Package":"example/bogus","Test":"TestAlsoBogus"}`,
		"FAIL\t" + testSyncPackage + "\t0.020s",
	}, "\n"))
	assert.Equal(t, []string{"TestDecode"}, raw.failedTopLevelTests())

	event := parseGoTestLog(`2026-11-11T10:20:00.5Z {"Action":"fail","Package":"` + testSyncPackage + `","Test":"TestEvent"}`)
	assert.Equal(t, []string{"TestEvent"}, event.failedTopLevelTests())
}

// Validates: R-6.10.23, R-6.10.26
//
// A test that hangs until go test's -timeout fires prints no --- FAIL line:
// the timeout panic lists it under "running tests:". Without reading that
// list the nightly keyed the failure by its job and `verify failed-test`
// reran the whole package. A test named only by the panic is not a verdict a
// classified rerun may answer.
func TestParseGoTestLog_TimeoutPanicNamesTheRunningTest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		log  string
	}{
		{
			name: "raw output, indented panic",
			log: strings.Join([]string{
				"=== RUN   TestHang",
				"=== RUN   TestOther/sub",
				"panic: test timed out after 10m0s",
				"\trunning tests:",
				"\t\tTestHang (10m0s)",
				"\t\tTestOther/sub (9m58s)",
				"",
				"goroutine 7 [running]:",
				"testing.(*M).startAlarm.func1()",
				"FAIL\t" + testSyncPackage + "\t600.123s",
			}, "\n"),
		},
		{
			name: "raw output behind CI timestamps, unindented panic",
			log: strings.Join([]string{
				"2026-11-11T10:20:00Z panic: test timed out after 10m0s",
				"2026-11-11T10:20:00Z running tests:",
				"2026-11-11T10:20:00Z \tTestHang (10m0s)",
				"2026-11-11T10:20:00Z \tTestOther/sub (9m58s)",
				"2026-11-11T10:20:00Z ",
				"2026-11-11T10:20:00Z goroutine 7 [running]:",
				"2026-11-11T10:20:01Z FAIL\t" + testSyncPackage + "\t600.123s",
			}, "\n"),
		},
		{
			name: "json output",
			log: strings.Join([]string{
				`{"Action":"run","Package":"` + testSyncPackage + `","Test":"TestHang"}`,
				`{"Action":"output","Package":"` + testSyncPackage + `","Test":"TestHang","Output":"panic: test timed out after 10m0s\n"}`,
				`{"Action":"output","Package":"` + testSyncPackage + `","Test":"TestHang","Output":"\trunning tests:\n"}`,
				`{"Action":"output","Package":"` + testSyncPackage + `","Test":"TestHang","Output":"\t\tTestHang (10m0s)\n"}`,
				`{"Action":"output","Package":"` + testSyncPackage + `","Test":"TestHang","Output":"\t\tTestOther/sub (9m58s)\n"}`,
				`{"Action":"output","Package":"` + testSyncPackage + `","Test":"TestHang","Output":"\n"}`,
				`{"Action":"output","Package":"` + testSyncPackage + `","Output":"FAIL\t` + testSyncPackage + `\t600.123s\n"}`,
				`{"Action":"fail","Package":"` + testSyncPackage + `","Elapsed":600.123}`,
			}, "\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log := parseGoTestLog(tc.log)
			assert.Equal(t, []string{"TestHang", "TestOther"}, log.failedTopLevelTests())
			assert.True(t, log.deadline)
			last, ok := log.lastFailure()
			require.True(t, ok)
			assert.Equal(t, goTestFailure{pkg: testSyncPackage, test: "TestOther/sub", timedOut: true}, last)
			assert.Empty(t, log.classifiableFailures(), "a hang is never a classified quirk")
		})
	}
}

// Validates: R-6.10.26
//
// A package that fails to build, or panics before any test reports, fails
// with no test named; the log names the package alone. A failure line a
// passing package printed (a test's own fixture text) is not a failure.
func TestParseGoTestLog_PackageFailuresAndPassingPackages(t *testing.T) {
	t.Parallel()

	const graphPackage = "github.com/tonimelisma/onedrive-go/internal/graph"
	log := parseGoTestLog(strings.Join([]string{
		"--- FAIL: TestPrintedByAFixture (0.00s)",
		"ok  \t" + graphPackage + "\t0.101s",
		"FAIL\t" + testSyncPackage + " [build failed]",
		`{"Action":"fail","Package":"` + testE2EPackage + `","Test":"TestE2E_Status_JSONShape"}`,
		`{"Action":"fail","Package":"` + testE2EPackage + `"}`,
	}, "\n"))

	assert.Equal(t, []string{"TestE2E_Status_JSONShape"}, log.failedTopLevelTests())
	assert.Equal(t, map[string]struct{}{"TestE2E_Status_JSONShape": {}}, log.classifiableFailures())
	last, ok := log.lastFailure()
	require.True(t, ok)
	assert.Equal(t, goTestFailure{pkg: testE2EPackage, test: "TestE2E_Status_JSONShape"}, last)

	build := parseGoTestLog("FAIL\t" + testSyncPackage + " [build failed]\n")
	last, ok = build.lastFailure()
	require.True(t, ok)
	assert.Equal(t, goTestFailure{pkg: testSyncPackage}, last)
	assert.Empty(t, build.failedTopLevelTests())

	_, ok = parseGoTestLog("ok  \t" + graphPackage + "\t0.101s\n").lastFailure()
	assert.False(t, ok)
}

// Validates: R-6.10.23, R-6.10.26
//
// Only go test's own result lines close a package: `ok`, two spaces, a tab
// and the package, or `FAIL`, a tab and the package. A CLI under test that
// prints a line starting with "ok" must not drop the failures read so far.
func TestParseGoTestLog_StrayOkLineDoesNotCloseARawPackage(t *testing.T) {
	t.Parallel()

	log := parseGoTestLog(strings.Join([]string{
		"--- FAIL: TestSyncReports (0.01s)",
		"ok then",
		"    sync_test.go:40: FAIL count=1",
		"FAIL\t" + testSyncPackage + "\t0.120s",
	}, "\n"))

	assert.Equal(t, []string{"TestSyncReports"}, log.failedTopLevelTests())
	last, ok := log.lastFailure()
	require.True(t, ok)
	assert.Equal(t, goTestFailure{pkg: testSyncPackage, test: "TestSyncReports"}, last)

	setup := parseGoTestLog("FAIL\t" + testSyncPackage + " [setup failed]\n")
	last, ok = setup.lastFailure()
	require.True(t, ok)
	assert.Equal(t, goTestFailure{pkg: testSyncPackage}, last)
}
