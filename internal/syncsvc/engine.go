package syncsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/macro-inc/macro/internal/syncsvc/lorowasm"
)

// docEngine is the CRDT boundary. The session/storage code only talks to this
// interface, so the wasm-backed implementation can be swapped for a Rust
// sidecar or a fake in tests. All methods are called with the document's
// advisory lock held and the session mutex taken — implementations need not
// be thread-safe.
type docEngine interface {
	// Import applies one or more updates (lorodoc_importBatch).
	ImportBatch(ctx context.Context, updates [][]byte) error
	// ExportSnapshot mirrors doc.export({mode:"snapshot"}).
	ExportSnapshot(ctx context.Context) ([]byte, error)
	// ExportShallowSnapshot mirrors the Rust initial-sync snapshot.
	ExportShallowSnapshot(ctx context.Context) ([]byte, error)
	// ExportUpdatesSince mirrors doc.export({mode:"update",from:vv}).
	ExportUpdatesSince(ctx context.Context, vv []byte) ([]byte, error)
	// VersionID mirrors DocumentState::version_id (oplog vv, hex-ish string).
	VersionID(ctx context.Context) (string, error)
	// ApplyAwareness applies an ephemeral-awareness update.
	ApplyAwareness(ctx context.Context, awareness []byte) error
	// EncodeAllAwareness mirrors EphemeralStore::encode_all().
	EncodeAllAwareness(ctx context.Context) ([]byte, error)
	// KeepOpsOnFlush reports whether the pending-op log must be preserved
	// when the snapshot is persisted. True only for the passthrough engine,
	// which stores ops verbatim without merging — deleting them on flush
	// would silently discard user edits.
	KeepOpsOnFlush() bool
	// Close releases engine resources.
	Close(ctx context.Context) error
}

// engineFactory builds a docEngine for a freshly-locked document.
// snapshot may be nil (new doc); pendingOps replay on top.
type engineFactory func(ctx context.Context, snapshot []byte, pendingOps [][]byte) (docEngine, error)

// --- lorowasm-backed engine ---------------------------------------------------

// loroEngine adapts a lorowasm.Instance to docEngine.
type loroEngine struct {
	inst      *lorowasm.Instance
	doc       *lorowasm.Doc
	ephemeral uint32 // EphemeralStoreWasm ptr (awareness), 0 if init failed
}

// newLoroEngineFactory compiles the artifact once and returns a factory that
// spawns a per-document wasm instance.
func newLoroEngineFactory(ctx context.Context, wasmPath string) (engineFactory, *lorowasm.Runtime, error) {
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read loro wasm: %w", err)
	}
	rt, err := lorowasm.NewRuntime(ctx, wasmBytes)
	if err != nil {
		return nil, nil, err
	}
	factory := func(ctx context.Context, snapshot []byte, pendingOps [][]byte) (docEngine, error) {
		inst, err := rt.NewInstance(ctx)
		if err != nil {
			return nil, err
		}
		e := &loroEngine{inst: inst}
		if len(snapshot) > 0 {
			doc, err := inst.DocFromSnapshot(ctx, snapshot)
			if err != nil {
				inst.Close(ctx)
				return nil, fmt.Errorf("load snapshot: %w", err)
			}
			e.doc = doc
		} else {
			doc, err := inst.NewDoc(ctx)
			if err != nil {
				inst.Close(ctx)
				return nil, err
			}
			e.doc = doc
		}
		if len(pendingOps) > 0 {
			if err := e.doc.ImportBatch(ctx, pendingOps); err != nil {
				inst.Close(ctx)
				return nil, fmt.Errorf("replay pending ops: %w", err)
			}
		}
		// EphemeralStore mirrors the Rust awareness store (30s timeout like
		// EphemeralStore::new default is not exposed; Rust uses default()).
		if eph, err := inst.NewEphemeral(ctx, 30_000); err == nil {
			e.ephemeral = eph
		}
		return e, nil
	}
	return factory, rt, nil
}

func (e *loroEngine) ImportBatch(ctx context.Context, updates [][]byte) error {
	return e.doc.ImportBatch(ctx, updates)
}

func (e *loroEngine) ExportSnapshot(ctx context.Context) ([]byte, error) {
	return e.doc.ExportSnapshot(ctx)
}

func (e *loroEngine) ExportShallowSnapshot(ctx context.Context) ([]byte, error) {
	return e.doc.ExportShallowSnapshot(ctx)
}

func (e *loroEngine) ExportUpdatesSince(ctx context.Context, vv []byte) ([]byte, error) {
	vvPtr, err := e.inst.VVDecode(ctx, vv)
	if err != nil {
		return nil, fmt.Errorf("decode vv: %w", err)
	}
	defer e.inst.FreeVV(ctx, vvPtr)
	return e.doc.ExportUpdatesSince(ctx, vvPtr)
}

func (e *loroEngine) VersionID(ctx context.Context) (string, error) {
	vvPtr, err := e.doc.OplogVV(ctx)
	if err != nil {
		return "", err
	}
	defer e.inst.FreeVV(ctx, vvPtr)
	vvBytes, err := e.inst.VVEncode(ctx, vvPtr)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(vvBytes), nil
}

func (e *loroEngine) ApplyAwareness(ctx context.Context, awareness []byte) error {
	if e.ephemeral == 0 {
		return nil // awareness store unavailable; tolerate
	}
	return e.inst.EphemeralApply(ctx, e.ephemeral, awareness)
}

func (e *loroEngine) EncodeAllAwareness(ctx context.Context) ([]byte, error) {
	if e.ephemeral == 0 {
		return nil, nil
	}
	return e.inst.EphemeralEncodeAll(ctx, e.ephemeral)
}

func (e *loroEngine) KeepOpsOnFlush() bool { return false }

func (e *loroEngine) Close(ctx context.Context) error {
	// Free the wasm-side Rust objects before tearing the module down —
	// without this the LoroDoc/EphemeralStore allocations leak for the
	// lifetime of the wasm instance.
	if e.ephemeral != 0 {
		e.inst.FreeEphemeral(ctx, e.ephemeral)
		e.ephemeral = 0
	}
	if e.doc != nil {
		e.doc.Free(ctx)
		e.doc = nil
	}
	e.inst.Close(ctx)
	return nil
}

// --- passthrough fallback ----------------------------------------------------

// passthroughEngine is the no-wasm fallback: it stores opaque bytes so the
// HTTP/WS/persistence plumbing runs E2E, but it cannot merge CRDT ops —
// ImportBatch stashes ops verbatim and Export* return the stored snapshot.
// Clearly inadequate for real sync; see REPORT.md.
type passthroughEngine struct {
	snapshot  []byte
	ops       [][]byte
	awareness []byte
}

func newPassthroughEngine(ctx context.Context, snapshot []byte, pendingOps [][]byte) (docEngine, error) {
	return &passthroughEngine{snapshot: snapshot, ops: pendingOps}, nil
}

func (e *passthroughEngine) ImportBatch(ctx context.Context, updates [][]byte) error {
	e.ops = append(e.ops, updates...)
	return nil
}

func (e *passthroughEngine) ExportSnapshot(ctx context.Context) ([]byte, error) {
	return e.snapshot, nil
}

func (e *passthroughEngine) ExportShallowSnapshot(ctx context.Context) ([]byte, error) {
	return e.snapshot, nil
}

func (e *passthroughEngine) ExportUpdatesSince(ctx context.Context, vv []byte) ([]byte, error) {
	return nil, errors.New("passthrough engine: cannot compute updates-since without CRDT")
}

func (e *passthroughEngine) VersionID(ctx context.Context) (string, error) {
	sum := sha256.Sum256(e.snapshot)
	return hex.EncodeToString(sum[:16]), nil
}

func (e *passthroughEngine) ApplyAwareness(ctx context.Context, awareness []byte) error {
	e.awareness = append(e.awareness[:0], awareness...)
	return nil
}

func (e *passthroughEngine) EncodeAllAwareness(ctx context.Context) ([]byte, error) {
	return e.awareness, nil
}

func (e *passthroughEngine) KeepOpsOnFlush() bool { return true }

func (e *passthroughEngine) Close(ctx context.Context) error { return nil }

// pickEngineFactory chooses lorowasm when an artifact is configured and
// readable, else the passthrough engine. Passthrough stores ops it cannot
// merge — returning success while silently degrading sync is worse than
// failing, so it is only allowed when explicitly opted in
// (SYNC_ALLOW_PASSTHROUGH) or under the SYNC_INSECURE_AUTH dev flag.
func pickEngineFactory(ctx context.Context, cfg syncConfig, logf func(string, ...any)) (engineFactory, *lorowasm.Runtime, error) {
	if cfg.LoroWasmPath != "" {
		factory, rt, err := newLoroEngineFactory(ctx, cfg.LoroWasmPath)
		if err != nil {
			logf("lorowasm init failed (%v)", err)
		} else {
			return factory, rt, nil
		}
	}
	if !cfg.AllowPassthrough && !cfg.InsecureAuth {
		return nil, nil, errors.New("no SYNC_LORO_WASM_PATH configured (or wasm init failed); " +
			"refusing to run the lossy passthrough engine — set SYNC_ALLOW_PASSTHROUGH=true " +
			"(or SYNC_INSECURE_AUTH=true for local dev) to override")
	}
	logf("WARNING: no SYNC_LORO_WASM_PATH configured; using passthrough engine — CRDT merge disabled, ops are stored but never merged")
	return newPassthroughEngine, nil, nil
}

// jsonOK marshals v or returns a 500-able error.
func jsonOK(v any) ([]byte, error) { return json.Marshal(v) }
