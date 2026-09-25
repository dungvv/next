package lorowasm

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// jsThrow is panicked inside a host function to emulate a JS throw; the wasm
// call boundary (Instance.call) recovers it and converts it into an error.
type jsThrow struct{ val any }

// hostFuncs maps import names the loro-crdt wasm-bindgen artifact needs
// (module "__wbindgen_placeholder__") to Go implementations. The names match
// `module.exports.__wbg_*` / `__wbindgen_*` in the bundled loro_wasm.js glue.
//
// Each host function resolves the calling module's JS heap via heapFor(mod),
// so one Runtime can host many document instances safely.
//
// Where the JS glue is behaviorally simple (property access, boxing,
// allocation) we mirror it exactly. Where it needs a real JS engine
// (Function constructor, promises, FinalizationRegistry) we stub and count —
// see Runtime.stubCalls.
func (r *Runtime) hostFuncs() map[string]api.GoModuleFunc {
	ret := func(stack []uint64, vals ...uint64) { copy(stack, vals) }
	u32 := func(v uint64) uint32 { return api.DecodeU32(v) }
	f64 := func(v uint64) float64 { return math.Float64frombits(v) }
	// num reads a stack slot that may encode i32 or f64 as a JS number.
	num := func(v uint64) float64 {
		if v < (1 << 32) {
			return float64(uint32(v))
		}
		return math.Float64frombits(v)
	}

	// passStr mirrors passStringToWasm0: malloc(len,1) + write.
	passStr := func(ctx context.Context, i *Instance, s string) (ptr, length uint32) {
		b := []byte(s)
		if len(b) == 0 {
			return 0, 0
		}
		p, err := i.malloc(ctx, uint32(len(b)), 1)
		if err != nil {
			return 0, 0
		}
		i.mem().Write(p, b)
		return p, uint32(len(b))
	}
	// writeStrOut mirrors the `(retptr, heapIdx) -> mem[retptr]=[ptr,len]`
	// pattern used by __wbindgen_string_get / debug_string / __wbg_String_.
	writeStrOut := func(ctx context.Context, i *Instance, stack []uint64, s string) {
		retptr := u32(stack[0])
		ptr, ln := passStr(ctx, i, s)
		i.mem().WriteUint32Le(retptr, ptr)
		i.mem().WriteUint32Le(retptr+4, ln)
	}
	// inst wraps the calling module so host code can use Instance helpers.
	inst := func(m api.Module) *Instance {
		return &Instance{rt: r, mod: m, heap: r.heapFor(m)}
	}

	return map[string]api.GoModuleFunc{
		// --- __wbindgen core -------------------------------------------------
		"__wbindgen_string_new": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			str, _ := i.readString(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(str)))
		},
		"__wbindgen_string_get": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			str, _ := i.heap.get(u32(s[1])).(string)
			writeStrOut(ctx, i, s, str)
		},
		"__wbindgen_debug_string": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			writeStrOut(ctx, i, s, jsDebugString(i.heap.get(u32(s[1]))))
		},
		"__wbindgen_number_get": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			var f float64
			ok := false
			if v, is := i.heap.get(u32(s[1])).(float64); is {
				f, ok = v, true
			}
			i.mem().WriteFloat64Le(u32(s[0])+8, f)
			i.mem().WriteUint32Le(u32(s[0]), boolU32(ok))
		},
		"__wbindgen_number_new": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(f64(s[0]))))
		},
		"__wbindgen_bigint_get_as_i64": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			var v int64
			ok := false
			switch t := i.heap.get(u32(s[1])).(type) {
			case jsBigint:
				v, ok = int64(t), true
			case jsUBig:
				v, ok = int64(t), true
			}
			i.mem().WriteUint64Le(u32(s[0])+8, uint64(v))
			i.mem().WriteUint32Le(u32(s[0]), boolU32(ok))
		},
		"__wbindgen_bigint_from_i64": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsBigint(int64(s[0])))))
		},
		"__wbindgen_bigint_from_u64": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsUBig(s[0]))))
		},
		"__wbindgen_boolean_get": func(ctx context.Context, m api.Module, s []uint64) {
			if v, is := inst(m).heap.get(u32(s[0])).(bool); is {
				ret(s, uint64(boolU32(v)))
			} else {
				ret(s, 2) // not a boolean
			}
		},
		"__wbindgen_as_number": func(ctx context.Context, m api.Module, s []uint64) {
			var f float64
			switch v := inst(m).heap.get(u32(s[0])).(type) {
			case float64:
				f = v
			case bool:
				if v {
					f = 1
				}
			case jsBigint:
				f = float64(v)
			}
			ret(s, math.Float64bits(f))
		},
		"__wbindgen_is_undefined": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(boolU32(inst(m).heap.get(u32(s[0])) == jsUndefined)))
		},
		"__wbindgen_is_null": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(boolU32(inst(m).heap.get(u32(s[0])) == jsNull)))
		},
		"__wbindgen_is_string": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(string)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbindgen_is_object": func(ctx context.Context, m api.Module, s []uint64) {
			v := inst(m).heap.get(u32(s[0]))
			ret(s, uint64(boolU32(jsKind(v) == "object" && v != jsNull)))
		},
		"__wbindgen_is_function": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(jsFn)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbindgen_is_array": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(*jsArray)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbindgen_is_bigint": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(boolU32(jsKind(inst(m).heap.get(u32(s[0]))) == "bigint")))
		},
		"__wbindgen_typeof": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsKind(i.heap.get(u32(s[0]))))))
		},
		"__wbindgen_in": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(boolU32(i.jsGet(i.heap.get(u32(s[1])), i.heap.get(u32(s[0]))) != jsUndefined)))
		},
		"__wbindgen_jsval_eq": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(boolU32(jsEq(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), false))))
		},
		"__wbindgen_jsval_loose_eq": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(boolU32(jsEq(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), true))))
		},
		"__wbindgen_object_clone_ref": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.heap.get(u32(s[0])))))
		},
		"__wbindgen_object_drop_ref": func(ctx context.Context, m api.Module, s []uint64) {
			inst(m).heap.drop(u32(s[0]))
		},
		"__wbindgen_error_new": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			msg, _ := i.readString(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(jsError{msg: msg})))
		},
		"__wbindgen_throw": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			msg, _ := i.readString(u32(s[0]), u32(s[1]))
			panic(jsThrow{val: jsError{msg: msg}})
		},
		"__wbindgen_rethrow": func(ctx context.Context, m api.Module, s []uint64) {
			panic(jsThrow{val: inst(m).heap.get(u32(s[0]))})
		},
		"__wbindgen_cb_drop": func(ctx context.Context, m api.Module, s []uint64) {
			inst(m).heap.drop(u32(s[0]))
			ret(s, 1)
		},
		"__wbindgen_memory": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsBuffer{})))
		},
		"__wbindgen_closure_wrapper283": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsFn{name: "closure283"})))
		},
		"__wbindgen_closure_wrapper542": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsFn{name: "closure542"})))
		},

		// --- object/array/reflect -------------------------------------------
		"__wbg_get_67b2ba62fc30de12": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.jsGet(i.heap.get(u32(s[0])), i.heap.get(u32(s[1]))))))
		},
		"__wbg_get_b9b93047fe3cf45b": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.jsGet(i.heap.get(u32(s[0])), float64(u32(s[1]))))))
		},
		"__wbg_getwithrefkey_1dc361bd10053bfe": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.jsGet(i.heap.get(u32(s[0])), i.heap.get(u32(s[1]))))))
		},
		"__wbg_getindex_5b00c274b05714aa": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			if f, ok := i.jsGet(i.heap.get(u32(s[0])), float64(u32(s[1]))).(float64); ok {
				ret(s, math.Float64bits(f))
			} else {
				ret(s, math.Float64bits(math.NaN()))
			}
		},
		"__wbg_set_37837023f3d740e8": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			jsSet(i.heap.get(u32(s[0])), float64(u32(s[1])), i.heap.take(u32(s[2])))
		},
		"__wbg_set_3f1d0b984ed272ed": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			jsSet(i.heap.get(u32(s[0])), i.heap.take(u32(s[1])), i.heap.take(u32(s[2])))
		},
		"__wbg_set_65595bdd868b3009": func(ctx context.Context, m api.Module, s []uint64) {
			// Map.set(key, u32)
			i := inst(m)
			if mp, ok := i.heap.get(u32(s[0])).(jsMap); ok {
				mp.set(i.heap.get(u32(s[1])), float64(u32(s[2])))
			}
		},
		"__wbg_set_8fc6bf8a5b1071d1": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			if mp, ok := i.heap.get(u32(s[0])).(jsMap); ok {
				ret(s, uint64(i.heap.add(mp.set(i.heap.get(u32(s[1])), i.heap.get(u32(s[2]))))))
				return
			}
			ret(s, uint64(i.heap.add(jsUndefined)))
		},
		"__wbg_set_bb8cecf6a62b9f46": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			jsSet(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), i.heap.get(u32(s[2])))
			ret(s, 1)
		},
		"__wbg_setindex_dcd71eabf405bde1": func(ctx context.Context, m api.Module, s []uint64) {
			// obj[u32] = f64 — plain array number store.
			i := inst(m)
			jsSet(i.heap.get(u32(s[0])), float64(u32(s[1])), f64(s[2]))
		},
		"__wbg_new_405e22f390576ce2": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsObject{})))
		},
		"__wbg_new_5e0be73521bc8c17": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(newJSMap())))
		},
		"__wbg_new_78feb108b6472713": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(&jsArray{})))
		},
		"__wbg_new_a12002a7f91c75be": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			// new Uint8Array(buffer) — whole-memory view (offset 0).
			ret(s, uint64(i.heap.add(&jsU8Array{offset: 0, length: i.mem().Size()})))
		},
		"__wbg_new_c68d7209be747379": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			msg, _ := i.readString(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(jsError{msg: msg})))
		},
		"__wbg_newnoargs_105ed471475aaf50": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			src, _ := i.readString(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(jsFn{name: "Function(" + src + ")"})))
		},
		"__wbg_newwithbyteoffsetandlength_d97e637ebe145a9a": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(&jsU8Array{offset: u32(s[1]), length: u32(s[2])})))
		},
		"__wbg_newwithlength_a381634e90c276d4": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(&jsU8Array{owned: make([]byte, u32(s[0]))})))
		},
		"__wbg_newwithlength_c4c419ef0bc8a1f8": func(ctx context.Context, m api.Module, s []uint64) {
			arr := jsArray(make([]any, u32(s[0])))
			ret(s, uint64(inst(m).heap.add(&arr)))
		},
		"__wbg_length_a446193dc22c12f8": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(jsLength(inst(m).heap.get(u32(s[0])))))
		},
		"__wbg_length_e2d2a49132c1b256": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(jsLength(inst(m).heap.get(u32(s[0])))))
		},
		"__wbg_buffer_609cc3eee51ed158": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsBuffer{})))
		},
		"__wbg_subarray_aa9065fa9dc5df96": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			switch a := i.heap.get(u32(s[0])).(type) {
			case *jsU8Array:
				beg, end := u32(s[1]), u32(s[2])
				if a.owned != nil {
					if int(end) > len(a.owned) {
						end = uint32(len(a.owned))
					}
					ret(s, uint64(i.heap.add(&jsU8Array{owned: a.owned[beg:end]})))
				} else {
					ret(s, uint64(i.heap.add(&jsU8Array{offset: a.offset + beg, length: end - beg})))
				}
			default:
				ret(s, 0)
			}
		},
		"__wbg_push_737cfc8c1432c2c6": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			if arr, ok := i.heap.get(u32(s[0])).(*jsArray); ok {
				*arr = append(*arr, i.heap.get(u32(s[1])))
				ret(s, uint64(len(*arr)))
				return
			}
			ret(s, 0)
		},
		"__wbg_from_2a5d3e218e67aa85": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			arr := jsIterable(i.heap.get(u32(s[0])))
			ret(s, uint64(i.heap.add(&arr)))
		},
		"__wbg_isArray_a1eab7e0d067391b": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(*jsArray)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbg_isSafeInteger_343e2beeeece1bb0": func(ctx context.Context, m api.Module, s []uint64) {
			f, ok := inst(m).heap.get(u32(s[0])).(float64)
			ret(s, uint64(boolU32(ok && f == math.Trunc(f) && math.Abs(f) <= 9007199254740991)))
		},
		"__wbg_entries_3265d4158b33e5dc": func(ctx context.Context, m api.Module, s []uint64) {
			// Object.entries(obj) -> [[k,v],...]
			i := inst(m)
			arr := jsArray{}
			if o, ok := i.heap.get(u32(s[0])).(jsObject); ok {
				for k, v := range o {
					arr = append(arr, jsArray{k, v})
				}
			}
			ret(s, uint64(i.heap.add(&arr)))
		},
		"__wbg_entries_c8a90a7ed73e84ce": func(ctx context.Context, m api.Module, s []uint64) {
			// obj.entries() -> iterator of [k,v]
			i := inst(m)
			ret(s, uint64(i.heap.add(&jsIter{vals: jsIterable(i.heap.get(u32(s[0])))})))
		},
		"__wbg_iterator_9a24c88df860dc65": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(jsSymbol("Symbol.iterator"))))
		},
		"__wbg_next_25feadfc0913fea9": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			// getObject(arg0).next — the method itself, not a call.
			ret(s, uint64(i.heap.add(jsGet(i.heap.get(u32(s[0])), "next"))))
		},
		"__wbg_next_6574e1a8a62d1055": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			it := i.heap.get(u32(s[0]))
			v := i.callJS(i.jsGet(it, "next"), it, nil) // iter.next()
			ret(s, uint64(i.heap.add(v)))
		},
		"__wbg_done_769e5ede4b31c67b": func(ctx context.Context, m api.Module, s []uint64) {
			done, _ := jsGet(inst(m).heap.get(u32(s[0])), "done").(bool)
			ret(s, uint64(boolU32(done)))
		},
		"__wbg_value_cd1ffa7b1ab794f1": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsGet(i.heap.get(u32(s[0])), "value"))))
		},
		"__wbg_ownKeys_3930041068756f1f": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			arr := jsArray{}
			if o, ok := i.heap.get(u32(s[0])).(jsObject); ok {
				for k := range o {
					arr = append(arr, k)
				}
			}
			ret(s, uint64(i.heap.add(&arr)))
		},
		"__wbg_getOwnPropertySymbols_97eebed6fe6e08be": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(&jsArray{})))
		},
		"__wbg_instanceof_ArrayBuffer_e14585432e3737fc": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(jsBuffer)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbg_instanceof_Map_f3469ce2244d2430": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(jsMap)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbg_instanceof_Object_7f2dcef8f78644a4": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(boolU32(jsKind(inst(m).heap.get(u32(s[0]))) == "object")))
		},
		"__wbg_instanceof_Uint8Array_17156bcf118086a9": func(ctx context.Context, m api.Module, s []uint64) {
			_, ok := inst(m).heap.get(u32(s[0])).(*jsU8Array)
			ret(s, uint64(boolU32(ok)))
		},
		"__wbg_String_8f0eb39a4a4c2f66": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			writeStrOut(ctx, i, s, jsToString(i.heap.get(u32(s[1]))))
		},

		// --- function calls ---------------------------------------------------
		"__wbg_call_672a4d21634d4a24": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.callJS(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), nil))))
		},
		"__wbg_call_7cccdd69e0791ae2": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.callJS(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), []any{i.heap.get(u32(s[2]))}))))
		},
		"__wbg_call_833bed5770ea2041": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.callJS(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), []any{i.heap.get(u32(s[2])), i.heap.get(u32(s[3]))}))))
		},
		"__wbg_call_b8adc8b1d0a0d8eb": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.callJS(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), []any{i.heap.get(u32(s[2])), i.heap.get(u32(s[3])), i.heap.get(u32(s[4]))}))))
		},
		"__wbg_apply_36be6a55257c99bf": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			var args []any
			if a, ok := i.heap.get(u32(s[2])).(*jsArray); ok {
				args = *a
			}
			ret(s, uint64(i.heap.add(i.callJS(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), args))))
		},
		"__wbg_apply_eb9e9b97497f91e4": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			var args []any
			if a, ok := i.heap.get(u32(s[2])).(*jsArray); ok {
				args = *a
			}
			ret(s, uint64(i.heap.add(i.callJS(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), args))))
		},
		"__wbg_resolve_4851785c9c5f573d": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.heap.get(u32(s[0]))))) // Promise.resolve(x) ~ x
		},
		"__wbg_then_44b73946d2fb3e7d": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, 0) // promises unsupported in spike
		},

		// --- globals / crypto / time ------------------------------------------
		"__wbg_static_accessor_GLOBAL_88a902d13a557d07": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(r.global)))
		},
		"__wbg_static_accessor_GLOBAL_THIS_56578be7e9f832b0": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(r.global)))
		},
		"__wbg_static_accessor_SELF_37c5d418e4bf5819": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(r.global)))
		},
		"__wbg_static_accessor_WINDOW_5de37043a91a9c40": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(r.global)))
		},
		"__wbg_crypto_574e78ad8b13b65f": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsGet(r.global, "crypto"))))
		},
		"__wbg_msCrypto_a61aeb35a24c1329": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsGet(r.global, "msCrypto"))))
		},
		"__wbg_process_dc0fbacc7c1c06f7": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsGet(r.global, "process"))))
		},
		"__wbg_versions_c01dfd4722a88165": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.jsGet(i.heap.get(u32(s[0])), "versions"))))
		},
		"__wbg_node_905d3e251edff8a2": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(i.jsGet(i.heap.get(u32(s[0])), "node"))))
		},
		"__wbg_require_60cc747a6bc5215a": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, uint64(inst(m).heap.add(r.requireFn)))
		},
		"__wbg_getRandomValues_b8f5dbd5f3995a9e": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			r.fillRandom(i, i.heap.get(u32(s[1])))
		},
		"__wbg_randomFillSync_ac0988aba3254290": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			r.fillRandom(i, i.heap.take(u32(s[1])))
		},
		"__wbg_now_454cbad00d40f177": func(ctx context.Context, m api.Module, s []uint64) {
			ret(s, math.Float64bits(float64(time.Now().UnixMilli())))
		},
		"__wbg_mark_7438147ce31e9d4b": func(ctx context.Context, m api.Module, s []uint64) {
		},
		"__wbg_measure_fb7825c11612c823": func(ctx context.Context, m api.Module, s []uint64) {
		},

		// --- console -----------------------------------------------------------
		"__wbg_log_0cc1b7768397bcfe": func(ctx context.Context, m api.Module, s []uint64) {
			r.logJS(inst(m).heap, "log", s)
		},
		"__wbg_log_cb9e190acc5753fb": func(ctx context.Context, m api.Module, s []uint64) {
			r.logJS(inst(m).heap, "log", s)
		},
		"__wbg_log_d5951556f2f10ec9": func(ctx context.Context, m api.Module, s []uint64) {
			r.logJS(inst(m).heap, "log", s)
		},
		"__wbg_warn_576423aac512a858": func(ctx context.Context, m api.Module, s []uint64) {
			r.logJS(inst(m).heap, "warn", s)
		},
		"__wbg_error_10d6926de361eb2b": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			msg, _ := i.readString(u32(s[0]), u32(s[1]))
			r.logf("console.error: %s", msg)
		},

		// --- loro snippets (state* tree builders) -------------------------------
		"__wbg_stateOptions_8486fef4478d16b3": func(ctx context.Context, m api.Module, s []uint64) {
			// stateOptions(options, doc?) — validation only; return options.
			ret(s, s[0])
		},
		"__wbg_stateSlice_8d4107ebb0dfc484": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			node := i.heap.get(u32(s[0]))
			ret(s, uint64(i.heap.add(jsObject{
				"cid":         jsGet(node, "cid"),
				"start":       f64(s[1]),
				"totalLength": f64(s[2]),
				"items":       jsGet(node, "value"),
			})))
		},
		"__wbg_stateContainer_71de82dd2efd60eb": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsObject{
				"type":  jsKindName(int(u32(s[0]))),
				"cid":   i.heap.get(u32(s[1])),
				"value": i.heap.get(u32(s[2])),
			})))
		},
		"__wbg_stateCid_760e963fd4c95f01": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			peer, _ := i.readString(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(fmt.Sprintf("cid:%d@%s:%s",
				int32(u32(s[2])), peer, jsKindName(int(u32(s[3])))))))
		},
		"__wbg_stateRootCid_964d9fbc9bfbcea3": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			name, _ := i.readString(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(fmt.Sprintf("cid:root-%s:%s", name, jsKindName(int(u32(s[2])))))))
		},
		"__wbg_stateValue_0727099e81830513": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			ret(s, uint64(i.heap.add(jsObject{"type": "Value", "value": i.heap.get(u32(s[0]))})))
		},
		"__wbg_stateBinary_64e9eb9b37720ab6": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			b, _ := i.readBytes(u32(s[0]), u32(s[1]))
			ret(s, uint64(i.heap.add(&jsU8Array{owned: b})))
		},
		"__wbg_stateIndex_acd88b869f874497": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			jsSet(i.heap.get(u32(s[0])), num(s[1]), i.heap.get(u32(s[2])))
		},
		"__wbg_stateSet_6cd1dd82536a3182": func(ctx context.Context, m api.Module, s []uint64) {
			i := inst(m)
			jsSet(i.heap.get(u32(s[0])), i.heap.get(u32(s[1])), i.heap.get(u32(s[2])))
		},

		// --- class wrappers: X.__wrap(ptr) — wasm passes a raw Rust ptr; the JS
		// wraps it in an object exposing __wbg_ptr. Mirror as jsObject.
		"__wbg_loromap_new":         wrapCtor(r),
		"__wbg_lorolist_new":        wrapCtor(r),
		"__wbg_loromovablelist_new": wrapCtor(r),
		"__wbg_lorotext_new":        wrapCtor(r),
		"__wbg_lorotree_new":        wrapCtor(r),
		"__wbg_lorotreenode_new":    wrapCtor(r),
		"__wbg_lorocounter_new":     wrapCtor(r),
		"__wbg_cursor_new":          wrapCtor(r),
		"__wbg_versionvector_new":   wrapCtor(r),
		"__wbg_changemodifier_new":  wrapCtor(r),
	}
}

var jsKinds = []string{"Map", "List", "MovableList", "Text", "Tree", "Counter"}

func jsKindName(i int) string {
	if i >= 0 && i < len(jsKinds) {
		return jsKinds[i]
	}
	return fmt.Sprint(i)
}

func boolU32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// wrapCtor returns a host function mirroring `Class.__wrap(arg0)`:
// it re-boxes a raw Rust object pointer as a JS object with __wbg_ptr.
func wrapCtor(r *Runtime) api.GoModuleFunc {
	return func(ctx context.Context, m api.Module, s []uint64) {
		s[0] = uint64(r.heapFor(m).add(jsObject{"__wbg_ptr": float64(api.DecodeU32(s[0]))}))
	}
}
