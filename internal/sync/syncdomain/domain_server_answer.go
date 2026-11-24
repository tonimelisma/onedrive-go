package syncdomain

// ServerAnswer is what a Graph failure means for the work around it: scope
// detection, result classification and issue naming all read it, so a quota
// answer or a refusal is recognized identically everywhere. The executor reads
// it from the failure once, when it builds the action's completion
// (syncexec.CompletionFromTrackedAction), and the completion keeps it; this
// package names the answers and imports no transport.
type ServerAnswer int

const (
	ServerAnswerNone ServerAnswer = iota
	ServerAnswerUnauthorized
	ServerAnswerForbidden
	ServerAnswerThrottled
	// ServerAnswerQuota is a full OneDrive: 507, or quotaLimitReached /
	// insufficientStorage or an "insufficient space" message under any
	// status (Business has returned it as 500 and 403).
	ServerAnswerQuota
	ServerAnswerServiceOutage
	// ServerAnswerUnsupported is OneDrive refusing an operation it does not
	// implement (501, 505, or a notSupported code under any status). It is a
	// stable answer about one request, not an outage, so it must never count
	// toward the service scope: counted, five refusals would block every
	// drive.
	ServerAnswerUnsupported
	ServerAnswerPathTooLong
	ServerAnswerRequestTimeout
	// ServerAnswerTransientConflict is 412, and 409 resourceModified: the
	// item changed between our read and our write (an mtime PATCH racing a
	// server-side post-upload update), so a retry against fresh truth works.
	ServerAnswerTransientConflict
	ServerAnswerTransientNotFound
	ServerAnswerLocked
	// ServerAnswerTransferURLRejected is a pre-authenticated upload or
	// download URL refusing its own credential (the graph client's pre-authenticated URL refusal).
	// Only that file's transfer is affected; it is retried with a new URL.
	ServerAnswerTransferURLRejected
	// ServerAnswerMalware is content OneDrive flagged as malware
	// (the graph client's malware refusal). It is a permanent per-file issue: probed as
	// a permission problem, the download's 403 would be labeled read-denied
	// and retried forever.
	ServerAnswerMalware
	ServerAnswerOther
)
