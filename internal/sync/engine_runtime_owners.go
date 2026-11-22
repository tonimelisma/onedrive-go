package sync

// The run's scheduling state (the dependency graph, the queued and running
// sets, the slow start, the held work and the active block scopes) is one
// syncsched.Scheduler, which the single runtime goroutine that owns the run
// writes and reads. What the run will report is runResults.

// runResults owns what the run will report when it finishes.
type runResults struct {
	succeeded int
	failed    int
	// keptForRetry counts the failures among failed that the engine keeps as
	// retry work it runs again by itself (failureKeptForRetry).
	keptForRetry int
	// awaitingUser counts the failures among failed that a condition only the
	// user can clear refused, recorded with that condition (failureAwaitsUser).
	awaitingUser int
	// superseded counts the actions the run gave up on because the truth
	// they were planned against changed; the next pass replans them.
	superseded int
	errors     []error
	summaries  []failureSummaryEntry

	// fatalAuthRecorded is set once this run has recorded that sign-in is
	// required. Every in-flight transfer that also gets a 401 reaches the
	// fatal path again while the run stops; the account is marked and the
	// condition reported once. Not reset per pass: it lasts for the run.
	fatalAuthRecorded bool
}
