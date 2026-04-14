package sync

// WatchMutationKind identifies a daemon-side durable intent mutation that must
// be executed by the owning watch loop instead of a second writable store.
type WatchMutationKind string

const (
	WatchMutationApproveHeldDeletes        WatchMutationKind = "approve_held_deletes"
	WatchMutationRequestConflictResolution WatchMutationKind = "request_conflict_resolution"
)

// WatchMutationRequest carries one control-plane mutation into the watch loop.
// The sender provides a buffered response channel and waits synchronously for
// the watch owner to finish the durable write.
type WatchMutationRequest struct {
	Kind       WatchMutationKind
	ConflictID string
	Resolution string
	Response   chan WatchMutationResponse
}

// WatchMutationResponse returns the outcome of a watch-owner mutation. Held
// delete approval uses only Err; conflict requests also return the durable
// request status from the store workflow.
type WatchMutationResponse struct {
	ConflictRequestResult ConflictRequestResult
	Err                   error
}

func (r *WatchMutationRequest) respond(response *WatchMutationResponse) {
	if r == nil || r.Response == nil {
		return
	}
	if response == nil {
		response = &WatchMutationResponse{}
	}

	r.Response <- *response
}
