package driveops

import (
	"context"
	"fmt"
	"log/slog"
	gosync "sync"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
)

// TreeWorkers is how many files a recursive get or put transfers at once,
// whatever the size of the tree. The walk queues at most as many more before
// it waits, so 2×TreeWorkers files are in flight or queued at any time.
const TreeWorkers = 4

// TreeEventKind is what happened to one file or folder of a tree transfer.
type TreeEventKind int

const (
	// TreeFolderCreated: a download created the local folder; an upload made
	// sure the remote folder exists, which it may have done before.
	TreeFolderCreated TreeEventKind = iota + 1
	// TreeFileDone: the file was transferred.
	TreeFileDone
	// TreeFileFailed: the file was not transferred. Its transfer failed or
	// panicked, its remote name was refused, or the run ended before its
	// transfer started.
	TreeFileFailed
	// TreeFolderFailed: the folder could not be listed, read or created, its
	// remote name was refused, or the run ended before the walk finished it.
	// The event stands for everything below the folder the walk did not
	// report on its own.
	TreeFolderFailed
)

// TreeEvent is one outcome of a tree transfer.
type TreeEvent struct {
	Kind TreeEventKind
	// RelPath is the file's or folder's path below the tree's root, with "/"
	// separators; the root itself is "".
	RelPath string
	// LocalPath is the local file or folder. For a remote name the download
	// refused, which is no local path, it is the local folder the item would
	// have gone into.
	LocalPath string
	// ItemID is the OneDrive item of an uploaded file or of a remote folder
	// an upload made sure of.
	ItemID string
	// Size is the bytes the file transfer moved (TreeFileDone).
	Size int64
	// HashVerified reports that the downloaded bytes matched OneDrive's hash
	// (TreeFileDone of a download).
	HashVerified bool
	// Err is why the file or folder failed (TreeFileFailed,
	// TreeFolderFailed). Its text names the path, so it reads on its own.
	Err error
}

// TreeSummary counts the events of a tree transfer.
type TreeSummary struct {
	Files   int   // TreeFileDone
	Folders int   // TreeFolderCreated
	Failed  int   // TreeFileFailed and TreeFolderFailed
	Bytes   int64 // the Size of every TreeFileDone
}

func (s *TreeSummary) count(event *TreeEvent) {
	switch event.Kind {
	case TreeFolderCreated:
		s.Folders++
	case TreeFileDone:
		s.Files++
		s.Bytes += event.Size
	case TreeFileFailed, TreeFolderFailed:
		s.Failed++
	}
}

// TreeRootError is a tree transfer that could not start: a download's root
// folder could not be listed, or an upload's could not be opened locally or
// made on OneDrive. Nothing was reported before it.
type TreeRootError struct {
	Err error
}

func (e *TreeRootError) Error() string { return e.Err.Error() }

func (e *TreeRootError) Unwrap() error { return e.Err }

// treeJob is one file the walk hands to a worker: the event that names it,
// and the transfer that moves it and fills in what it learned (Size,
// HashVerified, ItemID).
type treeJob struct {
	file     TreeEvent
	transfer func(ctx context.Context, file *TreeEvent) error
}

// treePool runs a tree transfer's files on TreeWorkers goroutines while the
// caller's goroutine walks the tree, and reports every event from one
// collector goroutine, in the order the events happen. The queue holds
// TreeWorkers files, so a walk that finds every worker busy and the queue
// full waits: that wait is what bounds a tree of any size. The call that
// starts the pool owns its goroutines; finish waits for every one of them.
type treePool struct {
	logger *slog.Logger
	// verb completes "Internal error while <verb> <path>" for a recovered
	// panic, and noun names the run in the error of a canceled one.
	verb, noun string
	onEvent    func(TreeEvent)
	jobs       chan treeJob
	events     chan TreeEvent
	workers    gosync.WaitGroup
	collector  gosync.WaitGroup
	// summary is written by the collector alone and read once the
	// collector has ended.
	summary TreeSummary

	// mu guards failure: the first panic outside a file's own transfer,
	// which fails the run.
	mu      gosync.Mutex
	failure error
}

// startTreePool starts the workers and the collector of one tree transfer.
// verb and noun name the transfer in its errors ("downloading", "folder
// download"); onEvent, which may be nil, receives every event.
func startTreePool(ctx context.Context, logger *slog.Logger, verb, noun string, onEvent func(TreeEvent)) *treePool {
	pool := &treePool{
		logger:  logger,
		verb:    verb,
		noun:    noun,
		onEvent: onEvent,
		jobs:    make(chan treeJob, TreeWorkers),
		events:  make(chan TreeEvent, TreeWorkers),
	}
	for range TreeWorkers {
		pool.workers.Go(func() {
			defer pool.recoverWorker()
			pool.work(ctx)
		})
	}
	pool.collector.Go(func() {
		defer pool.recoverCollector()
		pool.collect()
	})

	return pool
}

// walk runs walkTree, the caller's walk of the tree, on the caller's
// goroutine. A panic there fails the run, like a panic in reporting does,
// instead of leaving the workers and the collector waiting on a queue that
// nobody closes.
func (p *treePool) walk(walkTree func()) {
	defer func() {
		if r := recover(); r != nil {
			p.fail(panicguard.Recovered(p.logger, "while walking the "+p.noun, r))
		}
	}()

	walkTree()
}

// send queues job's file for a worker, waiting while every worker is busy
// and the queue is full. Once ctx has ended it reports the file as failed
// instead, never started, and returns false: the walk stops.
func (p *treePool) send(ctx context.Context, job treeJob) bool {
	if err := ctx.Err(); err != nil {
		p.report(failedFile(job.file, err))
		return false
	}

	select {
	case p.jobs <- job:
		return true
	case <-ctx.Done():
		p.report(failedFile(job.file, ctx.Err()))
		return false
	}
}

// report hands an event the walk itself decided (a folder's, a refused
// name's, or a file's it never queued) to the collector.
func (p *treePool) report(event TreeEvent) {
	p.events <- event
}

// reportWalked reports an event of kind the walk decided about the item at
// relPath and localPath.
func (p *treePool) reportWalked(kind TreeEventKind, relPath, localPath string, err error) {
	p.report(TreeEvent{Kind: kind, RelPath: relPath, LocalPath: localPath, Err: err})
}

// reportUnfinished reports a folder whose walk ctx's end cut short: the files
// in it, and the folders below it, that the walk had not reached.
func (p *treePool) reportUnfinished(ctx context.Context, relPath, localPath string) {
	p.reportWalked(TreeFolderFailed, relPath, localPath,
		fmt.Errorf("%s: stopped before the folder was finished: %w", localPath, ctx.Err()))
}

func (p *treePool) work(ctx context.Context) {
	for job := range p.jobs {
		p.events <- p.run(ctx, job)
	}
}

// run transfers one file and returns its event. A file a canceled run never
// started fails with the context's error, and a panic in a file's transfer
// is that file's failure while the rest of the tree goes on (R-6.3.10).
func (p *treePool) run(ctx context.Context, job treeJob) (event TreeEvent) {
	if err := ctx.Err(); err != nil {
		return failedFile(job.file, err)
	}

	defer func() {
		if r := recover(); r != nil {
			event = failedFile(job.file, panicguard.Recovered(p.logger, "while "+p.verb+" "+job.file.LocalPath, r))
		}
	}()

	event = job.file
	if err := job.transfer(ctx, &event); err != nil {
		return failedFile(job.file, err)
	}
	event.Kind = TreeFileDone

	return event
}

// failedFile is the failure of file, with err after its path.
func failedFile(file TreeEvent, err error) TreeEvent {
	file.Kind = TreeFileFailed
	file.Err = fmt.Errorf("%s: %w", file.LocalPath, err)

	return file
}

// collect counts every event and hands it to onEvent, until the last worker
// and the walk are done and events is closed.
func (p *treePool) collect() {
	deliver := p.onEvent != nil
	for event := range p.events {
		p.summary.count(&event)
		if deliver {
			deliver = p.deliver(event)
		}
	}
}

// deliver hands event to onEvent and reports whether it returned. After a
// panic there the events are still counted but no longer delivered, and the
// run fails with the panic.
func (p *treePool) deliver(event TreeEvent) (delivered bool) {
	defer func() {
		if r := recover(); r != nil {
			p.fail(panicguard.Recovered(p.logger, "while reporting the "+p.noun+" of "+event.LocalPath, r))
			delivered = false
		}
	}()

	p.onEvent(event)

	return true
}

// recoverWorker is a worker's panic boundary. run recovers a file's own
// panic, so only the loop around it could reach this one: the run fails, and
// the worker goes on draining the queue so the walk never waits for it.
func (p *treePool) recoverWorker() {
	if r := recover(); r != nil {
		err := panicguard.Recovered(p.logger, "in a "+p.noun+" worker", r)
		p.fail(err)
		for job := range p.jobs {
			p.events <- failedFile(job.file, err)
		}
	}
}

// recoverCollector is the collector's panic boundary. deliver recovers a
// panic of onEvent, so only the loop around it could reach this one: the run
// fails, and the collector goes on draining the events so no worker waits for
// it.
func (p *treePool) recoverCollector() {
	if r := recover(); r != nil {
		p.fail(panicguard.Recovered(p.logger, "while counting the "+p.noun, r))
		for range p.events {
		}
	}
}

func (p *treePool) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failure == nil {
		p.failure = err
	}
}

// finish ends the queue once the walk is done, waits for every worker and for
// the collector, and returns the summary with the run's error: a panic that
// failed it, else the end of ctx when the run was canceled.
func (p *treePool) finish(ctx context.Context) (TreeSummary, error) {
	close(p.jobs)
	p.workers.Wait()
	close(p.events)
	p.collector.Wait()

	p.mu.Lock()
	failure := p.failure
	p.mu.Unlock()

	if failure != nil {
		return p.summary, failure
	}
	if err := ctx.Err(); err != nil {
		return p.summary, fmt.Errorf("%s stopped: %w", p.noun, err)
	}

	return p.summary, nil
}

// joinRelPath is the path below a tree's root of the child name of the
// folder at parent.
func joinRelPath(parent, name string) string {
	if parent == "" {
		return name
	}

	return parent + "/" + name
}
