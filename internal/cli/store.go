package cli

import (
	"context"
	"path/filepath"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
	"github.com/frankbardon/aperture/storage/sqlite"
)

// buildStore constructs and initialises a model.Storage for a command, then
// seeds it. This is the manual constructor-DI seam the check and serve commands
// share: choose a backend from --store, run Setup, and load a model from --seed
// (or the embedded example when --seed is empty).
//
//   - storeDSN == ""  -> in-memory backend (storage/memory), ideal for the demo.
//   - storeDSN != ""  -> SQLite backend at the DSN (storage/sqlite).
//   - seedPath == ""  -> load the embedded example fixture (account "acme").
//   - seedPath != ""  -> load the file, format inferred from its extension.
//
// On any failure the partially constructed store is closed and the caller gets a
// coded error: the failure's OWN code when it has one, and APERTURE_BOOT only
// when it does not. See bootError for why that distinction is not cosmetic.
func buildStore(ctx context.Context, storeDSN, seedPath string) (model.Storage, error) {
	store, err := openStore(storeDSN)
	if err != nil {
		return nil, err
	}
	if err := store.Setup(ctx); err != nil {
		_ = store.Close()
		return nil, bootError("cli: storage setup failed", err)
	}
	if err := loadSeed(ctx, store, seedPath); err != nil {
		_ = store.Close()
		return nil, bootError("cli: seeding the model failed", err)
	}
	return store, nil
}

// bootError codes a startup failure as APERTURE_BOOT — but only when the failure
// does not already carry a code of its own.
//
// aerr.Wrap does NOT pass an existing code through: it builds a fresh CodedError
// with whatever code it is handed, and aerr.CodeOf reports the OUTERMOST one. So
// wrapping unconditionally re-stamps, and the pass-through is a property of the
// call site, not of the wrapper (the same idiom lives in provider/registry.go,
// storage/sqlite/sqlite.go, and delegation/delegation.go).
//
// Storage startup is exactly where that matters. Setup returns two specific,
// actionable refusals — APERTURE_STORAGE_SCHEMA_INCOMPATIBLE for a database
// written by an older build, and APERTURE_STORAGE_CONSTRAINT for a connection
// that is not enforcing foreign keys — and seeding surfaces the coded rejection
// of whichever entity the document got wrong. Re-stamping all of them
// APERTURE_BOOT replaces that with "aperture failed to start", whose registry
// fixups are "check your environment variables" and "confirm the backend is
// reachable": true of every startup failure there is, and no help with any of
// these. The CLI is the surface an operator meets an unreadable database
// through, so burying the code here is burying it everywhere it would be read.
//
// A bare error still gets APERTURE_BOOT, which is what the code is for: it says
// the process could not start, for a reason no layer below classified.
func bootError(msg string, err error) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrap(aerr.APERTURE_BOOT, msg, err)
}

// openStore selects the storage backend from the DSN.
func openStore(storeDSN string) (model.Storage, error) {
	if storeDSN == "" {
		return memory.New(), nil
	}
	store, err := sqlite.Open(storeDSN)
	if err != nil {
		return nil, bootError("cli: open sqlite store", err)
	}
	return store, nil
}

// loadSeed loads the model from the seed file, or the embedded example when no
// path is given.
func loadSeed(ctx context.Context, store model.Storage, seedPath string) error {
	if seedPath == "" {
		return seed.Load(ctx, store, seed.Example, seed.FormatYAML)
	}
	return seed.LoadFile(ctx, store, seedPath)
}

// seedDocument parses the seed model (the --seed file, or the embedded example
// when empty) into a Document so a command can read the sections Apply does not
// write to storage — the two object-source wiring sections, `providers:` and
// `objects:`, which BuildRegistry turns into one live registry. It mirrors
// loadSeed's file-vs-embedded choice.
func seedDocument(seedPath string) (*seed.Document, error) {
	if seedPath == "" {
		doc, err := seed.Parse(seed.Example, seed.FormatYAML)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: parsing the embedded seed failed", err)
		}
		return doc, nil
	}
	doc, err := seed.ParseFile(seedPath)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: parsing the seed file failed", err)
	}
	return doc, nil
}

// seedBaseDir is the directory declared provider paths resolve against: the
// seed file's directory, or "" (the process CWD) for the embedded seed.
func seedBaseDir(seedPath string) string {
	if seedPath == "" {
		return ""
	}
	return filepath.Dir(seedPath)
}
