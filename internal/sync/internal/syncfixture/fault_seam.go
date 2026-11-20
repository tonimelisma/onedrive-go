package syncfixture

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	stdsync "sync"
)

const FaultDriverName = "sqlite-fault"

// FaultCommit is the fragment that arms the next transaction commit.
const FaultCommit = "COMMIT"

var ErrInjectedFault = errors.New("injected fault: disk I/O error (SQLITE_FULL)")

type SqlFault struct {
	mu    stdsync.Mutex // guards match, skip, err and Hits
	match string
	// skip is how many matching statements still run before the fault fires.
	skip int
	err  error
	Hits int
}

// Arm makes the next statement whose SQL contains match (or the next commit,
// for faultCommit) fail with err. One shot: the fault disarms when it fires.
func (f *SqlFault) Arm(match string, err error) {
	f.ArmAfter(match, 0, err)
}

// ArmAfter is arm for the statement after the next skip matching ones, so a
// test can fail a statement in the middle of a run of identical ones.
func (f *SqlFault) ArmAfter(match string, skip int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.skip, f.err = match, skip, err
}

func (f *SqlFault) take(query string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.match == "" || !strings.Contains(query, f.match) {
		return nil
	}
	if f.skip > 0 {
		f.skip--
		return nil
	}
	err := f.err
	f.match, f.err = "", nil
	f.Hits++
	return err
}

func (f *SqlFault) Fired() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Hits
}

//nolint:gochecknoglobals // database/sql registers a driver once per process, and its connections find their fault by database path.
var (
	FaultDriverOnce stdsync.Once
	faultsMu        stdsync.Mutex // guards faultsByPath
	faultsByPath    = map[string]*SqlFault{}
)

func FaultForPath(dbPath string) *SqlFault {
	faultsMu.Lock()
	defer faultsMu.Unlock()
	f, ok := faultsByPath[dbPath]
	if !ok {
		f = &SqlFault{}
		faultsByPath[dbPath] = f
	}
	return f
}

// faultPathFromDSN recovers the database path from the store's DSN, the
// percent-encoded `file:` URI SqliteFileURI builds, which keys the fault a
// connection consults.
func faultPathFromDSN(dsn string) (string, error) {
	uri, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("fault driver: parse DSN: %w", err)
	}
	return uri.Path, nil
}

type FaultDriver struct{ Inner driver.Driver }

func (d *FaultDriver) Open(name string) (driver.Conn, error) {
	path, err := faultPathFromDSN(name)
	if err != nil {
		return nil, err
	}
	conn, err := d.Inner.Open(name)
	if err != nil {
		return nil, fmt.Errorf("fault driver: open: %w", err)
	}
	return &faultConn{Conn: conn, fault: FaultForPath(path)}, nil
}

// faultConn forwards to the real connection, consulting the fault first.
// database/sql prefers the context forms, which the SQLite driver
// implements, so these are the paths every store statement takes.
type faultConn struct {
	driver.Conn
	fault *SqlFault
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.fault.take(query); err != nil {
		return nil, err
	}
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err != nil {
		return nil, fmt.Errorf("fault driver: exec: %w", err)
	}
	return result, nil
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.fault.take(query); err != nil {
		return nil, err
	}
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, fmt.Errorf("fault driver: query: %w", err)
	}
	return rows, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.fault.take(query); err != nil {
		return nil, err
	}
	var (
		stmt driver.Stmt
		err  error
	)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, query)
	} else {
		stmt, err = c.Prepare(query)
	}
	if err != nil {
		return nil, fmt.Errorf("fault driver: prepare: %w", err)
	}
	return stmt, nil
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var (
		tx  driver.Tx
		err error
	)
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err = beginner.BeginTx(ctx, opts)
	} else {
		tx, err = c.Begin()
	}
	if err != nil {
		return nil, fmt.Errorf("fault driver: begin: %w", err)
	}
	return &faultTx{Tx: tx, fault: c.fault}, nil
}

func (c *faultConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		if err := pinger.Ping(ctx); err != nil {
			return fmt.Errorf("fault driver: ping: %w", err)
		}
	}
	return nil
}

type faultTx struct {
	driver.Tx
	fault *SqlFault
}

func (t *faultTx) Commit() error {
	if err := t.fault.take(FaultCommit); err != nil {
		// The statements ran; the commit is what failed. Roll back so the
		// connection is reusable, as SQLite would after a failed commit.
		if rbErr := t.Rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if err := t.Tx.Commit(); err != nil {
		return fmt.Errorf("fault driver: commit: %w", err)
	}
	return nil
}
