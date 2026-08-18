package sqlprovider

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A hand-rolled fake database, so every unit test in this package runs in plain
// `make test` with NO database present — CI has no service containers and this
// package deliberately imports no driver.
//
// The fake is a database/sql driver rather than a Querier implementation because
// Querier's methods return *sql.Rows, a type only database/sql can build. Going
// in through sql.Register therefore exercises the real thing: the real argument
// binding, the real scan conversions, and the real context plumbing, with only
// the canned driver.Values at the bottom being ours.

const fakeDriverName = "aperture-sqlprovider-fake"

func init() { sql.Register(fakeDriverName, fakeDriver{}) }

// script is the canned behaviour of one fake database, plus what it observed.
type script struct {
	cols     []string
	rows     [][]driver.Value
	queryErr error         // returned instead of rows
	rowsErr  error         // returned by Next in place of io.EOF, i.e. a mid-stream failure
	delay    time.Duration // makes a statement outlive a short timeout

	mu           sync.Mutex
	calls        int
	lastQuery    string
	lastArgs     []driver.Value
	lastDeadline time.Time
	hadDeadline  bool
}

func (s *script) record(ctx context.Context, query string, args []driver.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastQuery = query
	s.lastArgs = args
	s.lastDeadline, s.hadDeadline = ctx.Deadline()
}

func (s *script) observed() (calls int, query string, args []driver.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.lastQuery, s.lastArgs
}

func (s *script) deadline() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDeadline, s.hadDeadline
}

var (
	fakeSeq atomic.Int64
	fakeMu  sync.Mutex
	fakeReg = map[string]*script{}
)

// newFakeDB returns a *sql.DB wired to s. Each call gets its own DSN, so tests
// stay independent and can run in parallel.
func newFakeDB(t *testing.T, s *script) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("script-%d", fakeSeq.Add(1))
	fakeMu.Lock()
	fakeReg[dsn] = s
	fakeMu.Unlock()
	db, err := sql.Open(fakeDriverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		fakeMu.Lock()
		delete(fakeReg, dsn)
		fakeMu.Unlock()
	})
	return db
}

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	fakeMu.Lock()
	s, ok := fakeReg[dsn]
	fakeMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no fake script registered for %q", dsn)
	}
	return &fakeConn{s: s}, nil
}

type fakeConn struct{ s *script }

var (
	_ driver.Conn           = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
)

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fake: Prepare is not supported; the provider queries directly")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("fake: no transactions") }

func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.s.record(ctx, query, vals)

	if c.s.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.s.delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.s.queryErr != nil {
		return nil, c.s.queryErr
	}
	return &fakeRows{s: c.s}, nil
}

type fakeRows struct {
	s *script
	i int
}

func (r *fakeRows) Columns() []string { return r.s.cols }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.s.rows) {
		if r.s.rowsErr != nil {
			return r.s.rowsErr
		}
		return io.EOF
	}
	row := r.s.rows[r.i]
	r.i++
	copy(dest, row)
	return nil
}
