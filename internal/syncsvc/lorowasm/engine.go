package lorowasm

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Runtime owns a shared wazero runtime, the compiled loro_wasm_bg.wasm module,
// and a single set of host shims. Each document gets its own instantiated
// module + JS heap (see Doc); host functions resolve per-document state from
// the calling api.Module, so one Runtime can host many docs.
//
// Docs are NOT safe for concurrent use — wasm modules are single-threaded —
// callers must serialize per doc (the syncsvc session mutex does this).
type Runtime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule

	global    jsObject
	requireFn jsFn

	// mu guards heaps and stubCalls. Host functions only ever mutate the heap
	// of the calling module, and callers serialize per doc.
	mu        sync.Mutex
	heaps     map[api.Module]*jsHeap
	stubCalls map[string]int

	// Trace logs every host-function call (name + params) when true.
	Trace bool
	// Logf is optional diagnostics for stubbed/unsupported imports.
	Logf func(format string, args ...any)
}

// NewRuntime compiles wasmBytes (loro-crdt's loro_wasm_bg.wasm) and installs
// host shims for every imported function.
func NewRuntime(ctx context.Context, wasmBytes []byte) (*Runtime, error) {
	r := &Runtime{
		heaps:     map[api.Module]*jsHeap{},
		stubCalls: map[string]int{},
	}
	// NB: crypto.getRandomValues / require("crypto").randomFillSync get routed
	// through the __wbg_getRandomValues/__wbg_randomFillSync imports (which know
	// the calling module) rather than calling these jsFns directly — these are
	// only present so `crypto`/`process` properties resolve like Node's.
	r.global = jsObject{
		"crypto": jsObject{"getRandomValues": jsFn{name: "crypto.getRandomValues"}},
		"process": jsObject{
			"versions": jsObject{"node": "v20.0.0"},
		},
	}
	r.requireFn = jsFn{name: "require", call: func(h *jsHeap, _ any, args []any) any {
		if len(args) > 0 && args[0] == "crypto" {
			return jsObject{"randomFillSync": jsFn{name: "crypto.randomFillSync"}}
		}
		return jsUndefined
	}}

	r.rt = wazero.NewRuntime(ctx)
	compiled, err := r.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile loro wasm: %w", err)
	}
	r.compiled = compiled

	handlers := r.hostFuncs()
	byModule := map[string][]api.FunctionDefinition{}
	for _, def := range compiled.ImportedFunctions() {
		modName, _, _ := def.Import()
		byModule[modName] = append(byModule[modName], def)
	}
	for modName, defs := range byModule {
		b := r.rt.NewHostModuleBuilder(modName)
		for _, def := range defs {
			_, impName, _ := def.Import()
			fn, ok := handlers[impName]
			if !ok {
				fn = r.stubFn(impName)
			}
			b.NewFunctionBuilder().
				WithGoModuleFunction(r.traced(impName, fn), def.ParamTypes(), def.ResultTypes()).
				Export(impName)
		}
		if _, err := b.Instantiate(ctx); err != nil {
			return nil, fmt.Errorf("instantiate host module %q: %w", modName, err)
		}
	}
	return r, nil
}

// NewInstance instantiates a fresh wasm module with its own JS heap. Each
// document session needs its own instance.
func (r *Runtime) NewInstance(ctx context.Context) (*Instance, error) {
	mod, err := r.rt.InstantiateModule(ctx, r.compiled, wazero.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("instantiate loro wasm: %w", err)
	}
	h := newJSHeap()
	r.mu.Lock()
	r.heaps[mod] = h
	r.mu.Unlock()
	inst := &Instance{rt: r, mod: mod, heap: h}
	if mod.ExportedFunction("__wbindgen_start") != nil {
		if _, err := inst.call(ctx, "__wbindgen_start"); err != nil {
			return nil, fmt.Errorf("__wbindgen_start: %w", err)
		}
	}
	return inst, nil
}

// Close releases the wazero runtime and all instances.
func (r *Runtime) Close(ctx context.Context) error {
	if r.rt != nil {
		return r.rt.Close(ctx)
	}
	return nil
}

// StubReport lists how many times each unimplemented host import was invoked —
// the PoC evidence for which code paths still lack real shims.
func (r *Runtime) StubReport() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.stubCalls))
	for k, v := range r.stubCalls {
		out[k] = v
	}
	return out
}

func (r *Runtime) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runtime) heapFor(mod api.Module) *jsHeap {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.heaps[mod]
	if !ok {
		// Shouldn't happen; create so a stray host call can't crash.
		h = newJSHeap()
		r.heaps[mod] = h
	}
	return h
}

func (r *Runtime) dropHeap(mod api.Module) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.heaps, mod)
}

func (r *Runtime) traced(name string, fn api.GoModuleFunc) api.GoModuleFunc {
	return func(ctx context.Context, m api.Module, stack []uint64) {
		if r.Trace {
			r.logf("host call %s stack=%v", name, stack)
		}
		fn(ctx, m, stack)
	}
}

func (r *Runtime) stubFn(name string) api.GoModuleFunc {
	return func(ctx context.Context, m api.Module, stack []uint64) {
		r.mu.Lock()
		r.stubCalls[name]++
		n := r.stubCalls[name]
		r.mu.Unlock()
		if n <= 3 {
			r.logf("lorowasm: stubbed host import %s called (params=%v)", name, stack)
		}
	}
}

// fillRandom mirrors crypto.getRandomValues / randomFillSync on a Uint8Array
// heap object (either a wasm-memory view or an owned buffer).
func (r *Runtime) fillRandom(i *Instance, v any) {
	a, ok := v.(*jsU8Array)
	if !ok {
		r.logf("lorowasm: fillRandom on %s", jsDebugString(v))
		return
	}
	if a.owned != nil {
		_, _ = rand.Read(a.owned)
		return
	}
	buf := make([]byte, a.length)
	_, _ = rand.Read(buf)
	i.mem().Write(a.offset, buf)
}

func (r *Runtime) logJS(h *jsHeap, level string, stack []uint64) {
	args := make([]any, 0, len(stack))
	for _, p := range stack {
		args = append(args, h.get(api.DecodeU32(p)))
	}
	r.logf("console.%s: %v", level, args)
}

// --- Instance: one instantiated module = one document -----------------------

// Instance is one instantiated loro wasm module + its JS heap.
type Instance struct {
	rt   *Runtime
	mod  api.Module
	heap *jsHeap
}

// Close drops the instance's heap and closes the wasm module.
func (i *Instance) Close(ctx context.Context) {
	i.rt.dropHeap(i.mod)
	_ = i.mod.Close(ctx)
}

func (i *Instance) mem() api.Memory { return i.mod.Memory() }

func (i *Instance) readBytes(ptr, length uint32) ([]byte, bool) {
	if length == 0 {
		return nil, true
	}
	return i.mem().Read(ptr, length)
}

func (i *Instance) readString(ptr, length uint32) (string, bool) {
	b, ok := i.readBytes(ptr, length)
	return string(b), ok
}

// call invokes an exported wasm function, converting jsThrow panics from host
// functions (and wazero traps) into errors.
func (i *Instance) call(ctx context.Context, name string, params ...uint64) (res []uint64, err error) {
	defer func() {
		if r := recover(); r != nil {
			switch t := r.(type) {
			case jsThrow:
				err = fmt.Errorf("%s: js exception: %s", name, jsDebugString(t.val))
			default:
				err = fmt.Errorf("%s: panic: %v", name, r)
			}
		}
	}()
	fn := i.mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("missing wasm export %q", name)
	}
	res, err = fn.Call(ctx, params...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return res, nil
}

func (i *Instance) malloc(ctx context.Context, size, align uint32) (uint32, error) {
	res, err := i.call(ctx, "__wbindgen_malloc", uint64(size), uint64(align))
	if err != nil {
		return 0, err
	}
	return api.DecodeU32(res[0]), nil
}

func (i *Instance) free(ctx context.Context, ptr, size uint32) {
	_, _ = i.call(ctx, "__wbindgen_free", uint64(ptr), uint64(size), 1)
}

// freeObj calls a wasm-bindgen class destructor (`__wbg_<class>_free`) on a
// raw object pointer. The arity varies between wasm-bindgen versions, so the
// parameter count is read off the export and extra slots get zeros.
func (i *Instance) freeObj(ctx context.Context, export string, ptr uint32) {
	if ptr == 0 {
		return
	}
	fn := i.mod.ExportedFunction(export)
	if fn == nil {
		return
	}
	params := make([]uint64, len(fn.Definition().ParamTypes()))
	if len(params) == 0 {
		return
	}
	params[0] = uint64(ptr)
	_, _ = i.call(ctx, export, params...)
}

// retptr mirrors `wasm.__wbindgen_add_to_stack_pointer(-16)` + restore.
func (i *Instance) retptr(ctx context.Context) (uint32, error) {
	res, err := i.call(ctx, "__wbindgen_add_to_stack_pointer", api.EncodeI32(-16))
	if err != nil {
		return 0, err
	}
	return api.DecodeU32(res[0]), nil
}

func (i *Instance) dropRetptr(ctx context.Context) {
	_, _ = i.call(ctx, "__wbindgen_add_to_stack_pointer", 16)
}

// passBytes mirrors passArray8ToWasm0: malloc + write; returns (ptr,len).
func (i *Instance) passBytes(ctx context.Context, b []byte) (uint32, uint32, error) {
	if len(b) == 0 {
		return 0, 0, nil
	}
	ptr, err := i.malloc(ctx, uint32(len(b)), 1)
	if err != nil {
		return 0, 0, err
	}
	if !i.mem().Write(ptr, b) {
		return 0, 0, fmt.Errorf("wasm memory write out of bounds")
	}
	return ptr, uint32(len(b)), nil
}

// jsGet is jsGet plus Uint8Array element reads that need wasm memory access.
func (i *Instance) jsGet(obj, key any) any {
	if a, ok := obj.(*jsU8Array); ok {
		f, isNum := key.(float64)
		if !isNum {
			return jsGet(obj, key)
		}
		if a.owned != nil {
			if int(f) >= 0 && int(f) < len(a.owned) {
				return float64(a.owned[int(f)])
			}
			return jsUndefined
		}
		if b, ok := i.mem().ReadByte(a.offset + uint32(f)); ok && uint32(f) < a.length {
			return float64(b)
		}
		return jsUndefined
	}
	return jsGet(obj, key)
}

// callJS invokes a jsFn value as `fn.call(thisArg, args...)`.
func (i *Instance) callJS(fn, thisArg any, args []any) any {
	if f, ok := fn.(jsFn); ok && f.call != nil {
		return f.call(i.heap, thisArg, args)
	}
	i.rt.logf("lorowasm: callJS on non-function %s", jsDebugString(fn))
	return jsUndefined
}

// --- LoroDoc facade ---------------------------------------------------------

// Doc is a handle to a wasm-side LoroDoc (the raw Rust object pointer the JS
// glue stores as __wbg_ptr). Free must be called before the owning
// Instance is closed.
type Doc struct {
	i   *Instance
	ptr uint32
}

// Free releases the wasm-side LoroDoc via __wbg_lorodoc_free. Safe to call
// once; the handle is unusable afterwards.
func (d *Doc) Free(ctx context.Context) {
	d.i.freeObj(ctx, "__wbg_lorodoc_free", d.ptr)
	d.ptr = 0
}

// NewDoc mirrors `new LoroDoc()` — lorodoc_new() returns the raw doc pointer.
func (i *Instance) NewDoc(ctx context.Context) (*Doc, error) {
	res, err := i.call(ctx, "lorodoc_new")
	if err != nil {
		return nil, err
	}
	return &Doc{i: i, ptr: api.DecodeU32(res[0])}, nil
}

// DocFromSnapshot mirrors `LoroDoc.fromSnapshot(bytes)`.
func (i *Instance) DocFromSnapshot(ctx context.Context, snapshot []byte) (*Doc, error) {
	retptr, err := i.retptr(ctx)
	if err != nil {
		return nil, err
	}
	defer i.dropRetptr(ctx)
	ptr, ln, err := i.passBytes(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	defer i.free(ctx, ptr, ln) // the JS glue frees passArray8ToWasm0 input post-call
	if _, err := i.call(ctx, "lorodoc_fromSnapshot", uint64(retptr), uint64(ptr), uint64(ln)); err != nil {
		return nil, err
	}
	r0, _ := i.mem().ReadUint32Le(retptr)
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	r2, _ := i.mem().ReadUint32Le(retptr + 8)
	if r2 != 0 {
		return nil, fmt.Errorf("fromSnapshot: %s", jsDebugString(i.heap.get(r1)))
	}
	return &Doc{i: i, ptr: r0}, nil
}

// Import mirrors `doc.import(updateOrSnapshot)`; the ImportStatus object is
// left on the heap (we only check the error flag).
func (d *Doc) Import(ctx context.Context, data []byte) error {
	i := d.i
	retptr, err := i.retptr(ctx)
	if err != nil {
		return err
	}
	defer i.dropRetptr(ctx)
	ptr, ln, err := i.passBytes(ctx, data)
	if err != nil {
		return err
	}
	defer i.free(ctx, ptr, ln)
	if _, err := i.call(ctx, "lorodoc_import", uint64(retptr), uint64(d.ptr), uint64(ptr), uint64(ln)); err != nil {
		return err
	}
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	r2, _ := i.mem().ReadUint32Le(retptr + 8)
	if r2 != 0 {
		return fmt.Errorf("import: %s", jsDebugString(i.heap.get(r1)))
	}
	return nil
}

// ImportBatch mirrors `doc.importBatch(bytes[])` — used to replay the op log.
func (d *Doc) ImportBatch(ctx context.Context, updates [][]byte) error {
	i := d.i
	arr := jsArray{}
	for _, u := range updates {
		arr = append(arr, &jsU8Array{owned: append([]byte(nil), u...)})
	}
	retptr, err := i.retptr(ctx)
	if err != nil {
		return err
	}
	defer i.dropRetptr(ctx)
	arrIdx := i.heap.add(&arr)
	defer i.heap.drop(arrIdx)
	if _, err := i.call(ctx, "lorodoc_importBatch", uint64(retptr), uint64(d.ptr), uint64(arrIdx)); err != nil {
		return err
	}
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	r2, _ := i.mem().ReadUint32Le(retptr + 8)
	if r2 != 0 {
		return fmt.Errorf("importBatch: %s", jsDebugString(i.heap.get(r1)))
	}
	return nil
}

// Export mirrors `doc.export(mode)` where mode is a JS object like
// {"mode":"snapshot"} or {"mode":"update","from": <vv obj>}.
func (d *Doc) Export(ctx context.Context, mode jsObject) ([]byte, error) {
	i := d.i
	retptr, err := i.retptr(ctx)
	if err != nil {
		return nil, err
	}
	defer i.dropRetptr(ctx)
	modeIdx := i.heap.add(mode)
	defer i.heap.drop(modeIdx)
	if _, err := i.call(ctx, "lorodoc_export", uint64(retptr), uint64(d.ptr), uint64(modeIdx)); err != nil {
		return nil, err
	}
	r0, _ := i.mem().ReadUint32Le(retptr)
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	r2, _ := i.mem().ReadUint32Le(retptr + 8)
	r3, _ := i.mem().ReadUint32Le(retptr + 12)
	if r3 != 0 {
		return nil, fmt.Errorf("export: %s", jsDebugString(i.heap.get(r2)))
	}
	out, ok := i.readBytes(r0, r1)
	if !ok {
		return nil, fmt.Errorf("export: result out of bounds")
	}
	cp := append([]byte(nil), out...)
	i.free(ctx, r0, r1)
	return cp, nil
}

// ExportSnapshot is doc.export({mode:"snapshot"}).
func (d *Doc) ExportSnapshot(ctx context.Context) ([]byte, error) {
	return d.Export(ctx, jsObject{"mode": "snapshot"})
}

// ExportShallowSnapshot is doc.export({mode:"shallow-snapshot",frontiers})
// using the doc's own oplog frontiers — matching the Rust
// DocumentState::export_shallow_snapshot.
func (d *Doc) ExportShallowSnapshot(ctx context.Context) ([]byte, error) {
	frIdx, err := d.OplogFrontiers(ctx)
	if err != nil {
		return nil, err
	}
	defer d.i.heap.drop(frIdx)
	return d.Export(ctx, jsObject{"mode": "shallow-snapshot", "frontiers": d.i.heap.get(frIdx)})
}

// OplogVV mirrors doc.oplogVersion() -> VersionVector raw ptr. The caller
// must free it with Instance.FreeVV.
func (d *Doc) OplogVV(ctx context.Context) (uint32, error) {
	res, err := d.i.call(ctx, "lorodoc_oplogVersion", uint64(d.ptr))
	if err != nil {
		return 0, err
	}
	return api.DecodeU32(res[0]), nil
}

// FreeVV frees a wasm-allocated VersionVector (from OplogVV / VVDecode) via
// the generated __wbg_versionvector_free destructor.
func (i *Instance) FreeVV(ctx context.Context, vvPtr uint32) {
	i.freeObj(ctx, "__wbg_versionvector_free", vvPtr)
}

// FreeEphemeral frees an EphemeralStoreWasm created by NewEphemeral.
func (i *Instance) FreeEphemeral(ctx context.Context, ptr uint32) {
	i.freeObj(ctx, "__wbg_ephemeralstorewasm_free", ptr)
}

// OplogFrontiers mirrors doc.oplogFrontiers() -> JS array heap index.
func (d *Doc) OplogFrontiers(ctx context.Context) (uint32, error) {
	i := d.i
	retptr, err := i.retptr(ctx)
	if err != nil {
		return 0, err
	}
	defer i.dropRetptr(ctx)
	if _, err := i.call(ctx, "lorodoc_oplogFrontiers", uint64(retptr), uint64(d.ptr)); err != nil {
		return 0, err
	}
	r0, _ := i.mem().ReadUint32Le(retptr)
	return r0, nil
}

// PeerIDStr mirrors doc.peerIdStr() — the doc's peer id as a decimal string.
func (d *Doc) PeerIDStr(ctx context.Context) (string, error) {
	res, err := d.i.call(ctx, "lorodoc_peerIdStr", uint64(d.ptr))
	if err != nil {
		return "", err
	}
	idx := api.DecodeU32(res[0])
	s, _ := d.i.heap.get(idx).(string)
	d.i.heap.drop(idx)
	return s, nil
}

// VVEncode mirrors VersionVector.encode() -> Uint8Array.
func (i *Instance) VVEncode(ctx context.Context, vvPtr uint32) ([]byte, error) {
	retptr, err := i.retptr(ctx)
	if err != nil {
		return nil, err
	}
	defer i.dropRetptr(ctx)
	if _, err := i.call(ctx, "versionvector_encode", uint64(retptr), uint64(vvPtr)); err != nil {
		return nil, err
	}
	r0, _ := i.mem().ReadUint32Le(retptr)
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	out, ok := i.readBytes(r0, r1)
	if !ok {
		return nil, fmt.Errorf("vv encode: out of bounds")
	}
	cp := append([]byte(nil), out...)
	i.free(ctx, r0, r1)
	return cp, nil
}

// VVDecode mirrors VersionVector.decode(bytes) -> raw vv ptr. The caller
// must free it with FreeVV — the wasm-side VersionVector leaks otherwise.
func (i *Instance) VVDecode(ctx context.Context, b []byte) (uint32, error) {
	retptr, err := i.retptr(ctx)
	if err != nil {
		return 0, err
	}
	defer i.dropRetptr(ctx)
	ptr, ln, err := i.passBytes(ctx, b)
	if err != nil {
		return 0, err
	}
	defer i.free(ctx, ptr, ln)
	if _, err := i.call(ctx, "versionvector_decode", uint64(retptr), uint64(ptr), uint64(ln)); err != nil {
		return 0, err
	}
	r0, _ := i.mem().ReadUint32Le(retptr)
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	r2, _ := i.mem().ReadUint32Le(retptr + 8)
	if r2 != 0 {
		return 0, fmt.Errorf("vv decode: %s", jsDebugString(i.heap.get(r1)))
	}
	return r0, nil
}

// ExportUpdatesSince mirrors doc.export({mode:"update", from: vv}) where vv is
// a VersionVector heap object carrying __wbg_ptr.
func (d *Doc) ExportUpdatesSince(ctx context.Context, vvPtr uint32) ([]byte, error) {
	return d.Export(ctx, jsObject{"mode": "update", "from": vvAsHeapObj(vvPtr)})
}

// vvAsHeapObj wraps a raw VersionVector ptr as the JS object shape the glue
// expects ({"__wbg_ptr": n}).
func vvAsHeapObj(ptr uint32) jsObject {
	return jsObject{"__wbg_ptr": float64(ptr)}
}

// --- EphemeralStore (awareness) ---------------------------------------------

// NewEphemeral mirrors `new EphemeralStoreWasm(timeout)` —
// ephemeralstorewasm_new(timeout f64) returns the raw ptr directly.
func (i *Instance) NewEphemeral(ctx context.Context, timeoutMs float64) (uint32, error) {
	res, err := i.call(ctx, "ephemeralstorewasm_new", api.EncodeF64(timeoutMs))
	if err != nil {
		return 0, err
	}
	return api.DecodeU32(res[0]), nil
}

// EphemeralApply mirrors store.apply(bytes) — returns the {updated,added}
// heap object on success (caller may drop it).
func (i *Instance) EphemeralApply(ctx context.Context, ptr uint32, data []byte) error {
	retptr, err := i.retptr(ctx)
	if err != nil {
		return err
	}
	defer i.dropRetptr(ctx)
	bptr, ln, err := i.passBytes(ctx, data)
	if err != nil {
		return err
	}
	defer i.free(ctx, bptr, ln)
	if _, err := i.call(ctx, "ephemeralstorewasm_apply", uint64(retptr), uint64(ptr), uint64(bptr), uint64(ln)); err != nil {
		return err
	}
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	r2, _ := i.mem().ReadUint32Le(retptr + 8)
	if r2 != 0 {
		return fmt.Errorf("ephemeral apply: %s", jsDebugString(i.heap.get(r1)))
	}
	return nil
}

// EphemeralEncodeAll mirrors store.encodeAll() -> Uint8Array.
func (i *Instance) EphemeralEncodeAll(ctx context.Context, ptr uint32) ([]byte, error) {
	retptr, err := i.retptr(ctx)
	if err != nil {
		return nil, err
	}
	defer i.dropRetptr(ctx)
	if _, err := i.call(ctx, "ephemeralstorewasm_encodeAll", uint64(retptr), uint64(ptr)); err != nil {
		return nil, err
	}
	r0, _ := i.mem().ReadUint32Le(retptr)
	r1, _ := i.mem().ReadUint32Le(retptr + 4)
	out, ok := i.readBytes(r0, r1)
	if !ok {
		return nil, fmt.Errorf("ephemeral encodeAll: out of bounds")
	}
	cp := append([]byte(nil), out...)
	i.free(ctx, r0, r1)
	return cp, nil
}

// SortedKeys is a debug helper for tests.
func SortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
