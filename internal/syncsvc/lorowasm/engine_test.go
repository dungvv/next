package lorowasm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// loadWasm locates loro_wasm_bg.wasm: set LORO_WASM_PATH, or drop the artifact
// into internal/syncsvc/lorowasm/testdata/. `npm pack loro-crdt` →
// package/nodejs/loro_wasm_bg.wasm.
func loadWasm(t *testing.T) []byte {
	t.Helper()
	if p := os.Getenv("LORO_WASM_PATH"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return b
		}
	}
	for _, p := range []string{
		"testdata/loro_wasm_bg.wasm",
		filepath.Join("testdata", "loro_wasm_bg.wasm"),
	} {
		if b, err := os.ReadFile(p); err == nil {
			return b
		}
	}
	t.Skip("no loro wasm artifact: set LORO_WASM_PATH or drop loro_wasm_bg.wasm in testdata/")
	return nil
}

func TestImportEnumeration(t *testing.T) {
	wasmBytes := loadWasm(t)
	ctx := context.Background()
	rt, err := NewRuntime(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer rt.Close(ctx)
	t.Logf("runtime ready; imported fns covered")
}

// TestDocRoundTrip exercises the operations the Rust sync-service's
// DocumentState needs: new doc, snapshot export/import, version-vector
// encode/decode, update export since a vv, shallow snapshot.
func TestDocRoundTrip(t *testing.T) {
	wasmBytes := loadWasm(t)
	ctx := context.Background()
	rt, err := NewRuntime(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer rt.Close(ctx)
	rt.Logf = t.Logf

	inst, err := rt.NewInstance(ctx)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer inst.Close(ctx)

	doc, err := inst.NewDoc(ctx)
	if err != nil {
		t.Fatalf("lorodoc_new: %v", err)
	}
	t.Logf("doc ptr=0x%x", doc.ptr)
	if doc.ptr == 0 {
		t.Fatal("lorodoc_new returned null")
	}
	if pid, err := doc.PeerIDStr(ctx); err != nil {
		t.Logf("peerIdStr failed (non-fatal): %v", err)
	} else {
		t.Logf("peer id: %s", pid)
	}

	snap, err := doc.ExportSnapshot(ctx)
	if err != nil {
		t.Fatalf("export snapshot: %v", err)
	}
	t.Logf("snapshot: %d bytes", len(snap))
	if len(snap) == 0 {
		t.Fatal("empty snapshot")
	}

	// Snapshot -> new doc -> export again (round-trip).
	doc2, err := inst.DocFromSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("fromSnapshot: %v", err)
	}
	snap2, err := doc2.ExportSnapshot(ctx)
	if err != nil {
		t.Fatalf("re-export snapshot: %v", err)
	}
	t.Logf("re-exported snapshot: %d bytes", len(snap2))
	if len(snap2) == 0 {
		t.Fatal("empty re-exported snapshot")
	}
	if err := doc2.Import(ctx, snap); err != nil {
		t.Fatalf("import snapshot: %v", err)
	}

	// oplog vv -> encode -> decode -> export updates since.
	vvPtr, err := doc2.OplogVV(ctx)
	if err != nil {
		t.Fatalf("oplog vv: %v", err)
	}
	vvBytes, err := inst.VVEncode(ctx, vvPtr)
	if err != nil {
		t.Fatalf("vv encode: %v", err)
	}
	vvPtr2, err := inst.VVDecode(ctx, vvBytes)
	if err != nil {
		t.Fatalf("vv decode: %v", err)
	}
	upd, err := doc2.ExportUpdatesSince(ctx, vvPtr2)
	if err != nil {
		t.Fatalf("export updates since vv: %v", err)
	}
	t.Logf("updates since vv: %d bytes", len(upd))

	// Shallow snapshot path (used for new-peer initial sync in Rust service).
	shallow, err := doc2.ExportShallowSnapshot(ctx)
	if err != nil {
		t.Fatalf("export shallow snapshot: %v", err)
	}
	t.Logf("shallow snapshot: %d bytes", len(shallow))

	// Second doc in same runtime must not share JS heap state.
	inst2, err := rt.NewInstance(ctx)
	if err != nil {
		t.Fatalf("instantiate #2: %v", err)
	}
	defer inst2.Close(ctx)
	doc3, err := inst2.NewDoc(ctx)
	if err != nil {
		t.Fatalf("new doc on instance 2: %v", err)
	}
	if _, err := doc3.ExportSnapshot(ctx); err != nil {
		t.Fatalf("export on instance 2: %v", err)
	}

	if n := len(rt.StubReport()); n > 0 {
		t.Logf("stubbed imports invoked: %v", rt.StubReport())
	}
}
