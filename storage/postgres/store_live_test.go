package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/storagetest"
)

// The statement set's live tests. Same gate as postgres_live_test.go — see its
// header — so `make test` passes with no database present and a gated run with
// an empty DSN fails rather than skips:
//
//	APERTURE_PG_INTEGRATION=1 \
//	APERTURE_PG_DSN='postgres://aperture@127.0.0.1:5432/aperture?sslmode=disable' \
//	go test -run TestPostgresLive ./storage/postgres/
//
// TestPostgresLive_Conformance is the one that matters: storagetest is the
// SINGLE behavioural contract every model.Storage backend satisfies, and it
// carries no backend-conditional assertions, so passing it is the whole of the
// claim that this backend and storage/sqlite are the same store.

// liveStore builds a Setup-completed Store in a scratch schema of its own, which
// is what gives each conformance subtest an independent world.
func liveStore(t *testing.T, dsn string) model.Storage {
	t.Helper()
	ctx := liveCtx(t)
	_, storeDSN := scratchSchema(t, ctx, dsn)
	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return s
}

// TestPostgresLive_Conformance runs the shared contract against a real server.
func TestPostgresLive_Conformance(t *testing.T) {
	dsn := requirePostgres(t)
	storagetest.Run(t, func(t *testing.T) model.Storage { return liveStore(t, dsn) })
}

// TestPostgresLive_ConformanceWithAConfiguredSchema runs the SAME contract
// against a Store whose tables are reachable ONLY through the schema qualifier.
// The store is configured with WithSchema and connects with search_path pointing
// somewhere else entirely (public), so nothing it does can resolve through
// session state: Setup creates the schema and its tables, and every subsequent
// statement finds them only because the qualifier was substituted in when the
// statement was built.
//
// Together with TestPostgresLive_Conformance above, this is the "full storagetest
// with a configured schema AND with none" criterion — one contract, no
// backend-conditional assertions, run twice over the two configurations.
//
// It is also the only way to prove the seam holds statement by statement. Every
// statement in this backend is written apt_schema.apt_<thing> and substituted
// before execution; one that FORGOT the qualifier would pass every unqualified
// test in this package silently, and fail here immediately with SQLSTATE 42P01,
// naming itself.
//
// This test used to reach into Store.qualifier directly, because there was no
// constructor that could set it. There is now, and it goes through the validator
// — so the pinned path the contract exercises is exactly the pinned path an
// operator gets, rather than a hand-assembled lookalike.
func TestPostgresLive_ConformanceWithAConfiguredSchema(t *testing.T) {
	dsn := requirePostgres(t)
	storagetest.Run(t, func(t *testing.T) model.Storage {
		ctx := liveCtx(t)
		name := reservedSchemaName(t, ctx, dsn)

		s, err := Open(withSearchPath(t, dsn, "public"), WithSchema(name))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Setup(ctx); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return s
	})
}

// TestPostgresLive_NestedAtomicDoesNotDeadlockOnASmallPool is the flattening
// claim, stated as the failure it prevents.
//
// A nested Atomic that opened a SECOND transaction would take a second
// connection out of the pool while the first is still held by the outer
// transaction. With a pool of one and a nesting depth of three that is an
// immediate self-deadlock: level two waits for a connection only level one can
// release, and level one waits for level two to return. The pool is capped at
// one here deliberately — the point is that flattening makes nesting cost
// exactly one connection at ANY depth, so the cap is a magnifying glass rather
// than a special case.
//
// The context deadline is what turns a regression into a FAILURE rather than a
// hung test run: database/sql waits for a free connection until the context
// expires, so a non-flattening implementation reports "context deadline
// exceeded" in a few seconds instead of blocking the suite forever.
func TestPostgresLive_NestedAtomicDoesNotDeadlockOnASmallPool(t *testing.T) {
	dsn := requirePostgres(t)
	setupCtx := liveCtx(t)
	_, storeDSN := scratchSchema(t, setupCtx, dsn)

	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Setup(setupCtx); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Smaller than the nesting depth below, on purpose.
	s.pool.SetMaxOpenConns(1)
	s.pool.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = s.Atomic(ctx, func(tx1 model.Storage) error {
		if tx1 == model.Storage(s) {
			t.Errorf("the outer Atomic handed back the root Store; fn must run against a transaction-scoped one")
		}
		if err := tx1.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}); err != nil {
			return err
		}
		return tx1.Atomic(ctx, func(tx2 model.Storage) error {
			// Flattening is observable: the nested call reuses the SAME
			// transaction-scoped Store rather than minting a second one.
			if tx2 != tx1 {
				t.Errorf("the nested Atomic did not flatten: it handed back a different Storage, "+
					"which means a second transaction and a second connection (%p vs %p)", tx2, tx1)
			}
			return tx2.Atomic(ctx, func(tx3 model.Storage) error {
				if tx3 != tx1 {
					t.Errorf("the third Atomic level did not flatten either")
				}
				return tx3.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"})
			})
		})
	})
	if err != nil {
		t.Fatalf("nested Atomic over a one-connection pool: %v\n"+
			"a deadline here is the deadlock this test exists to catch: a nested Atomic took a "+
			"second connection instead of reusing the open transaction", err)
	}

	// The whole nest committed once, through the outer transaction.
	if _, err := s.GetAccount(ctx, "acme"); err != nil {
		t.Errorf("the innermost write did not commit with the outer transaction: %v", err)
	}
	if _, err := s.GetObjectType(ctx, "document"); err != nil {
		t.Errorf("the outer write did not commit: %v", err)
	}

	// And a rollback at any depth still covers everything, because there is only
	// ever one transaction to roll back.
	sentinel := errRollback{}
	err = s.Atomic(ctx, func(tx1 model.Storage) error {
		if err := tx1.PutAccount(ctx, model.Account{ID: "doomed", Name: "Doomed"}); err != nil {
			return err
		}
		return tx1.Atomic(ctx, func(tx2 model.Storage) error { return sentinel })
	})
	if err == nil {
		t.Fatalf("an inner failure did not fail the outer Atomic")
	}
	if _, err := s.GetAccount(ctx, "doomed"); err == nil {
		t.Errorf("a write from a rolled-back nest survived, so the nested Atomic committed independently")
	}
}

type errRollback struct{}

func (errRollback) Error() string { return "rollback, deliberately" }

// ---- offline: flattening needs no server to state ----

// panicExec is a sqlExec that fails loudly if anything runs against it. It lets
// the flattening path below be exercised with no database at all: the whole
// claim is that a transaction-scoped Store begins nothing and executes nothing
// of its own.
type panicExec struct{ t *testing.T }

func (p panicExec) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	p.t.Fatalf("a flattened Atomic executed a statement of its own")
	return nil, nil
}
func (p panicExec) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	p.t.Fatalf("a flattened Atomic ran a query of its own")
	return nil, nil
}
func (p panicExec) QueryRowContext(context.Context, string, ...any) *sql.Row {
	p.t.Fatalf("a flattened Atomic ran a query of its own")
	return nil
}

// TestAtomicFlattensOnATransactionScopedStore is the offline half of the
// flattening claim, and it runs in `make test`: a Store with no pool must hand
// fn ITSELF and take nothing from anywhere.
func TestAtomicFlattensOnATransactionScopedStore(t *testing.T) {
	s := &Store{pool: nil, exec: panicExec{t: t}, schema: "aperture", qualifier: qualifierFor("aperture")}
	var got model.Storage
	if err := s.Atomic(context.Background(), func(tx model.Storage) error {
		got = tx
		return nil
	}); err != nil {
		t.Fatalf("Atomic: %v", err)
	}
	if got != model.Storage(s) {
		t.Errorf("a nested Atomic did not reuse the open transaction: got %p, want %p", got, s)
	}
}

// TestInTxFlattensOnATransactionScopedStore is the same property for the
// multi-statement writes (PutPrincipal, DeleteAccount, ...): they must join the
// enclosing transaction rather than begin one.
func TestInTxFlattensOnATransactionScopedStore(t *testing.T) {
	exec := panicExec{t: t}
	s := &Store{pool: nil, exec: exec}
	var got sqlExec
	if err := s.inTx(context.Background(), "op", func(tx sqlExec) error {
		got = tx
		return nil
	}); err != nil {
		t.Fatalf("inTx: %v", err)
	}
	if got != sqlExec(exec) {
		t.Errorf("inTx did not run against the enclosing transaction's exec")
	}
}

// TestStatementsCarryTheSchemaQualifier is the offline half of the seam: a
// statement built by this backend must come out addressing the configured
// schema. (That Atomic's transaction-scoped child inherits the qualifier is
// proved live, by running the whole contract — Atomic subtests included —
// against a pinned store.)
func TestStatementsCarryTheSchemaQualifier(t *testing.T) {
	pinned := &Store{schema: "aperture", qualifier: qualifierFor("aperture")}
	if got := pinned.q(grantSelect); !strings.Contains(got, `FROM "aperture".apt_grants`) {
		t.Errorf("a pinned Store built %q", got)
	}
	ambient := &Store{}
	got := ambient.q(grantSelect)
	if strings.Contains(got, schemaPlaceholder) {
		t.Errorf("the empty qualifier left %q in a statement: %q", schemaPlaceholder, got)
	}
	if !strings.Contains(got, "FROM apt_grants") {
		t.Errorf("the empty qualifier did not produce an unqualified statement: %q", got)
	}
}
