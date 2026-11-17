package devtool

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
)

// The first two lines of a data race report and its last: the race detector
// opens and closes each report with goTestRaceReportRule.
const (
	goTestRaceReportRule   = "=================="
	goTestRaceReportHeader = "WARNING: DATA RACE"
)

// goTestTextWriter turns the event stream of `go test -json` back into the
// text go test prints without -v, for a log a person reads: a package's own
// output as it comes; a test's output once that test fails, and of a test
// that passes or skips only the race reports in it; before a package's result
// line the output of each of its tests that never ended (cut short by a
// deadline or a crash); and build output. A line that is not an event is
// written as it is.
//
// test2json credits every line without a framing marker, a race report or a
// timeout's panic with its goroutine dump, to the test the last framing line
// named: often one that has printed its own `--- PASS:` line, and from a
// parallel test's `=== PAUSE` line to the next framing line one that waits
// paused. Such output cannot be that test's own, so it is written at once.
// Whatever reads the run itself reads the events the caller kept;
// parseGoTestLog finds the same failures, deadline and shuffle seeds in this
// text as in them.
//
// go test streams several packages' events interleaved, and one package's
// text stays together, as go test prints it, so a failure line reaches its
// own package's result line: the first package with an event streams, and
// another holds its text until the packages before it have ended. One go
// test writes its events from one goroutine, so the writer needs no lock.
type goTestTextWriter struct {
	out io.Writer
	// partial is the start of a line whose newline has not arrived yet.
	partial []byte
	// packages are the packages that have not written all their text, in the
	// order of their first event; the first streams.
	packages []*goTestPackageText
	// running holds the output of each test that started and has not ended,
	// in the order the tests started.
	running []*goTestRunOutput
}

// goTestPackageText is one package's text, held while a package before it
// has not ended.
type goTestPackageText struct {
	pkg   string
	held  strings.Builder
	ended bool
}

// goTestRunOutput is the output of one running test.
type goTestRunOutput struct {
	pkg    string
	test   string
	output strings.Builder
	// paused is set from the test's pause event to its cont event, and
	// reported once its own `--- PASS:` or `--- SKIP:` line came: output
	// credited to the test then is not its own.
	paused   bool
	reported bool
}

func newGoTestTextWriter(out io.Writer) *goTestTextWriter {
	return &goTestTextWriter{out: out}
}

// Write reads every whole line of p and keeps the rest for the next write.
func (w *goTestTextWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	consumed := 0
	for {
		end := bytes.IndexByte(w.partial[consumed:], '\n')
		if end < 0 {
			break
		}
		line := string(w.partial[consumed : consumed+end+1])
		consumed += end + 1
		if err := w.writeLine(line); err != nil {
			w.partial = append(w.partial[:0], w.partial[consumed:]...)
			return 0, err
		}
	}
	w.partial = append(w.partial[:0], w.partial[consumed:]...)

	return len(p), nil
}

// Flush writes what the stream left once go test exited: a last line without
// its newline, then package by package the text each held and the output of
// every test of it that never ended, as a run killed before its packages'
// results leaves them.
func (w *goTestTextWriter) Flush() error {
	if len(w.partial) > 0 {
		line := string(w.partial)
		w.partial = w.partial[:0]
		if err := w.writeLine(line); err != nil {
			return err
		}
	}
	for len(w.packages) > 0 {
		if err := w.endPackage(w.packages[0]); err != nil {
			return err
		}
	}

	return nil
}

// writeLine writes what one line of the stream stands for.
func (w *goTestTextWriter) writeLine(line string) error {
	_, event, isEvent := decodeGoTestLogLine(line)
	if !isEvent {
		return w.write(line)
	}
	if event.Action == goTestActionBuildOutput {
		return w.write(event.Output)
	}
	if event.Package == "" {
		return nil
	}
	pkg := w.packageText(event.Package)

	switch event.Action {
	case goTestActionRun:
		if event.Test != "" && w.find(event.Package, event.Test) < 0 {
			w.running = append(w.running, &goTestRunOutput{pkg: event.Package, test: event.Test})
		}
	case goTestActionPause, goTestActionCont:
		if i := w.find(event.Package, event.Test); i >= 0 {
			w.running[i].paused = event.Action == goTestActionPause
		}
	case goTestActionOutput:
		return w.writeOutput(pkg, event)
	case goTestActionPass, goTestActionSkip, goTestActionFail:
		if event.Test == "" {
			return w.endPackage(pkg)
		}
		return w.endTest(pkg, event)
	}

	return nil
}

// writeOutput holds a running test's own output until the test ends and
// writes any other at once: the package's own, a test's after it ended, and
// what test2json credits to a test that is paused or has printed its result
// line. A test's own result line writes the race reports it held. go test's
// result line of a package comes after the output of the tests the binary
// left unfinished, as go test without -v prints them.
func (w *goTestTextWriter) writeOutput(pkg *goTestPackageText, event goTestJSONEvent) error {
	if event.Test == "" {
		if isGoTestResultLine(event.Output) {
			if err := w.writeUnfinishedOf(pkg); err != nil {
				return err
			}
		}
		return w.writeTo(pkg, event.Output)
	}
	i := w.find(event.Package, event.Test)
	if i < 0 || w.running[i].paused || w.running[i].reported {
		return w.writeTo(pkg, event.Output)
	}
	run := w.running[i]
	if !isOwnPassOrSkipLine(event.Output, run.test) {
		run.output.WriteString(event.Output)
		return nil
	}
	run.reported = true
	races := goTestRaceReports(run.output.String())
	run.output.Reset()

	return w.writeTo(pkg, races)
}

// endTest drops a test that ended from the running ones, and writes its
// output when it failed, or the race reports in it when it passed or
// skipped.
func (w *goTestTextWriter) endTest(pkg *goTestPackageText, event goTestJSONEvent) error {
	i := w.find(event.Package, event.Test)
	if i < 0 {
		return nil
	}
	ended := w.running[i]
	w.running = slices.Delete(w.running, i, i+1)
	if event.Action == goTestActionFail {
		return w.writeTo(pkg, ended.output.String())
	}

	return w.writeTo(pkg, goTestRaceReports(ended.output.String()))
}

// endPackage writes the output of pkg's tests that never ended and marks it
// ended, then writes the text each package held from the first on, up to the
// first that has not ended, which streams from then on.
func (w *goTestTextWriter) endPackage(pkg *goTestPackageText) error {
	if err := w.writeUnfinishedOf(pkg); err != nil {
		return err
	}
	pkg.ended = true
	for len(w.packages) > 0 {
		first := w.packages[0]
		if err := w.write(first.held.String()); err != nil {
			return err
		}
		first.held.Reset()
		if !first.ended {
			return nil
		}
		w.packages = w.packages[1:]
	}

	return nil
}

// packageText is the text of pkg, a new one at the end for a package not
// seen yet.
func (w *goTestTextWriter) packageText(pkg string) *goTestPackageText {
	if i := slices.IndexFunc(w.packages, func(p *goTestPackageText) bool { return p.pkg == pkg }); i >= 0 {
		return w.packages[i]
	}
	text := &goTestPackageText{pkg: pkg}
	w.packages = append(w.packages, text)

	return text
}

// writeUnfinishedOf writes, and forgets, the output of pkg's tests that never
// ended, in the order they started.
func (w *goTestTextWriter) writeUnfinishedOf(pkg *goTestPackageText) error {
	var unfinished []*goTestRunOutput
	w.running = slices.DeleteFunc(w.running, func(run *goTestRunOutput) bool {
		if run.pkg == pkg.pkg {
			unfinished = append(unfinished, run)
			return true
		}
		return false
	})
	for _, run := range unfinished {
		if err := w.writeTo(pkg, run.output.String()); err != nil {
			return err
		}
	}

	return nil
}

// find is the index of a running test, or -1.
func (w *goTestTextWriter) find(pkg string, test string) int {
	return slices.IndexFunc(w.running, func(run *goTestRunOutput) bool {
		return run.pkg == pkg && run.test == test
	})
}

// writeTo writes text of pkg, or holds it while a package before pkg has not
// ended.
func (w *goTestTextWriter) writeTo(pkg *goTestPackageText, text string) error {
	if len(w.packages) > 0 && w.packages[0] != pkg {
		pkg.held.WriteString(text)
		return nil
	}

	return w.write(text)
}

func (w *goTestTextWriter) write(text string) error {
	if text == "" {
		return nil
	}
	if _, err := io.WriteString(w.out, text); err != nil {
		return fmt.Errorf("write go test text: %w", err)
	}

	return nil
}

// isGoTestResultLine reports whether a line of a package's output is go
// test's result line of a package.
func isGoTestResultLine(output string) bool {
	text := strings.TrimRight(output, "\r\n")
	_, failed := goTestResultPackage(text, rawGoTestPackageFailPrefix)
	_, passed := goTestResultPackage(text, rawGoTestPackagePassPrefix)

	return failed || passed
}

// isOwnPassOrSkipLine reports whether output is test's own result line of a
// pass or a skip, `--- PASS: <test> (<time>)`, indented for a subtest.
func isOwnPassOrSkipLine(output string, test string) bool {
	line := strings.TrimLeft(output, " ")

	return strings.HasPrefix(line, rawGoTestPassPrefix+" "+test+" (") || strings.HasPrefix(line, rawGoTestSkipPrefix+" "+test+" (")
}

// goTestRaceReports is every data race report in a test's output, each from
// goTestRaceReportRule and goTestRaceReportHeader to the closing rule, or to
// the end of the output.
func goTestRaceReports(output string) string {
	var reports strings.Builder
	inReport := false
	opening := ""
	for line := range strings.Lines(output) {
		text := strings.TrimRight(line, "\r\n")
		switch {
		case inReport:
			reports.WriteString(line)
			inReport = text != goTestRaceReportRule
			continue
		case text == goTestRaceReportHeader && opening != "":
			reports.WriteString(opening + line)
			inReport = true
		}
		opening = ""
		if text == goTestRaceReportRule {
			opening = line
		}
	}

	return reports.String()
}
