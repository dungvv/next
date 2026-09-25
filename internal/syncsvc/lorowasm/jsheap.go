// Package lorowasm is a feasibility spike for running the loro-crdt
// wasm-bindgen artifact (loro_wasm_bg.wasm from the `loro-crdt` npm package)
// inside Go via wazero.
//
// The artifact was compiled for a JS host: it imports ~118 host functions
// from the module "__wbindgen_placeholder__" and expects a JS-side heap where
// JsValues are exchanged by i32 index. This package reimplements just enough
// of that heap — mirroring the glue in loro-crdt's bundled loro_wasm.js — to
// instantiate the module and drive document create/export/import/version
// operations without a JS runtime.
package lorowasm

import (
	"fmt"
	"math"
	"sort"
)

// JS value model. wasm-bindgen (pre-externref ABI, which is what loro-crdt
// ships) passes every JS value across the boundary as an i32 index into a
// host-side heap. The first 128 slots are reserved; 128-131 are
// undefined/null/true/false; slot 0 reads as undefined.
type jsSentinel string

var (
	jsUndefined = jsSentinel("undefined")
	jsNull      = jsSentinel("null")
)

func (s jsSentinel) String() string { return string(s) }

type (
	jsObject map[string]any       // plain JS object (also class wrappers via __wbg_ptr)
	jsArray  []any                // JS Array contents; heap stores *jsArray
	jsBigint int64                // JS BigInt (i64)
	jsUBig   uint64               // JS BigInt (u64, kept distinct for typeof)
	jsError  struct{ msg string } // JS Error
	jsSymbol string               // Symbol.iterator etc.
	jsBuffer struct{}             // ArrayBuffer — always wasm linear memory here
	jsIter   struct {
		vals []any
		i    int
	} // iterator state
	jsFn struct {
		name string
		// call mirrors fn.call(thisArg, args...); nil means unimplemented.
		call func(h *jsHeap, thisArg any, args []any) any
	}
	// jsU8Array models a Uint8Array — either a view into wasm linear memory
	// (created via __wbg_newwithbyteoffsetandlength) or a standalone buffer
	// (new Uint8Array(n)). Views are dereferenced lazily through
	// Engine.mem() so they survive memory.grow.
	jsU8Array struct {
		owned  []byte // owned buffer when non-nil
		offset uint32 // byte offset into wasm memory when owned == nil
		length uint32 // length when owned == nil
	}
	// jsMap models JS Map closely enough for the state*/diff paths.
	jsMap struct{ m map[any]any }
)

func jsKind(v any) string {
	switch v.(type) {
	case jsSentinel:
		if v == jsNull {
			return "object"
		}
		return "undefined"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case jsBigint, jsUBig:
		return "bigint"
	case jsFn:
		return "function"
	default:
		return "object"
	}
}

func jsTruthy(v any) bool {
	switch t := v.(type) {
	case jsSentinel:
		return false // undefined/null
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	case jsBigint:
		return t != 0
	default:
		return true
	}
}

// jsHeap mirrors the freelist heap in loro_wasm.js:
//
//	const heap = new Array(128).fill(undefined);
//	heap.push(undefined, null, true, false); // 128..131
//	let heap_next = heap.length;             // 132
type jsHeap struct {
	heap     []any
	heapNext uint32
	// freed guards against wasm double-drops: the upstream JS freelist would
	// silently corrupt (heap[idx]=idx self-loop) on a double free; we detect
	// and ignore it instead.
	freed map[uint32]bool
}

func newJSHeap() *jsHeap {
	h := &jsHeap{heap: make([]any, 128), freed: map[uint32]bool{}}
	for i := range h.heap {
		h.heap[i] = jsUndefined
	}
	h.heap = append(h.heap, jsUndefined, jsNull, true, false)
	h.heapNext = uint32(len(h.heap))
	return h
}

func (h *jsHeap) get(idx uint32) any {
	if int(idx) >= len(h.heap) {
		return jsUndefined
	}
	return h.heap[idx]
}

func (h *jsHeap) set(idx uint32, v any) {
	if int(idx) < len(h.heap) {
		h.heap[idx] = v
	}
}

func (h *jsHeap) add(obj any) uint32 {
	if h.heapNext == uint32(len(h.heap)) {
		h.heap = append(h.heap, len(h.heap)+1)
	}
	idx := h.heapNext
	next, ok := h.heap[idx].(int)
	if !ok {
		// Defensive: the freelist link was clobbered — grow instead.
		h.heap = append(h.heap, obj)
		h.heapNext = uint32(len(h.heap))
		return uint32(len(h.heap) - 1)
	}
	h.heapNext = uint32(next)
	h.heap[idx] = obj
	delete(h.freed, idx)
	return idx
}

func (h *jsHeap) drop(idx uint32) {
	if idx < 132 || int(idx) >= len(h.heap) || h.freed[idx] {
		return
	}
	h.freed[idx] = true
	h.heap[idx] = int(h.heapNext)
	h.heapNext = idx
}

func (h *jsHeap) take(idx uint32) any {
	v := h.get(idx)
	h.drop(idx)
	return v
}

// jsGet mirrors Reflect.get / obj[key] for the shapes we support.
func jsGet(obj, key any) any {
	switch o := obj.(type) {
	case jsObject:
		if s, ok := key.(string); ok {
			if v, ok := o[s]; ok {
				return v
			}
			return jsUndefined
		}
		return jsUndefined
	case *jsArray:
		switch k := key.(type) {
		case float64:
			if int(k) >= 0 && int(k) < len(*o) {
				return (*o)[int(k)]
			}
		case string:
			if k == "length" {
				return float64(len(*o))
			}
		case jsSymbol:
			if k == "Symbol.iterator" {
				vals := *o
				return jsFn{name: "Symbol.iterator", call: func(h *jsHeap, _ any, _ []any) any {
					return &jsIter{vals: vals}
				}}
			}
		}
		return jsUndefined
	case jsMap:
		if s, ok := key.(string); ok && s == "size" {
			return float64(len(o.m))
		}
		return o.get(key)
	case *jsU8Array:
		if s, ok := key.(string); ok {
			if s == "length" {
				return float64(o.len())
			}
			if s == "buffer" {
				return jsBuffer{}
			}
		}
		return jsUndefined
	case string:
		if s, ok := key.(string); ok && s == "length" {
			return float64(len(s))
		}
		return jsUndefined
	case *jsIter:
		if s, ok := key.(string); ok && s == "next" {
			it := o
			return jsFn{name: "next", call: func(h *jsHeap, _ any, _ []any) any {
				if it.i >= len(it.vals) {
					return jsObject{"value": jsUndefined, "done": true}
				}
				v := it.vals[it.i]
				it.i++
				return jsObject{"value": v, "done": false}
			}}
		}
		return jsUndefined
	default:
		return jsUndefined
	}
}

func jsSet(obj, key, val any) {
	switch o := obj.(type) {
	case jsObject:
		if s, ok := key.(string); ok {
			o[s] = val
		}
	case *jsArray:
		if k, ok := key.(float64); ok {
			i := int(k)
			for len(*o) <= i {
				*o = append(*o, jsUndefined)
			}
			(*o)[i] = val
		}
	case jsMap:
		o.set(key, val)
	}
}

func newJSMap() jsMap { return jsMap{m: map[any]any{}} }

func (m jsMap) get(k any) any {
	if v, ok := m.m[k]; ok {
		return v
	}
	return jsUndefined
}

func (m jsMap) set(k, v any) jsMap {
	m.m[k] = v
	return m
}

func (a *jsU8Array) len() int {
	if a.owned != nil {
		return len(a.owned)
	}
	return int(a.length)
}

// jsEq approximates ===/== for the value types we track.
func jsEq(a, b any, loose bool) bool {
	if (a == jsUndefined && b == jsNull) || (a == jsNull && b == jsUndefined) {
		return loose
	}
	switch av := a.(type) {
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case jsBigint:
			return loose && av == float64(bv)
		}
	case jsBigint:
		switch bv := b.(type) {
		case jsBigint:
			return av == bv
		case jsUBig:
			return loose && av == jsBigint(bv)
		case float64:
			return loose && float64(av) == bv
		}
	case jsUBig:
		if bv, ok := b.(jsUBig); ok {
			return av == bv
		}
	}
	return a == b
}

func jsDebugString(v any) string {
	switch t := v.(type) {
	case jsSentinel:
		return string(t)
	case string:
		return fmt.Sprintf("%q", t)
	case float64:
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%v", t)
	case jsObject:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Sprintf("object{%v}", keys)
	case *jsArray:
		return fmt.Sprintf("array[%d]", len(*t))
	case jsError:
		return "Error: " + t.msg
	case jsFn:
		return "function " + t.name
	case *jsU8Array:
		return fmt.Sprintf("Uint8Array[%d]", t.len())
	case jsMap:
		return fmt.Sprintf("Map[%d]", len(t.m))
	default:
		return fmt.Sprintf("%T", v)
	}
}

func jsLength(v any) uint32 {
	switch t := v.(type) {
	case string:
		return uint32(len(t))
	case *jsArray:
		return uint32(len(*t))
	case *jsU8Array:
		return uint32(t.len())
	case jsMap:
		return uint32(len(t.m))
	default:
		return 0
	}
}

// jsIterable materializes a value as a list — mirrors Array.from and the
// iterator fast paths used by importBatch/diff.
func jsIterable(v any) jsArray {
	switch t := v.(type) {
	case *jsArray:
		return *t
	case jsMap:
		arr := make(jsArray, 0, len(t.m))
		for k, val := range t.m {
			arr = append(arr, jsArray{k, val})
		}
		return arr
	case jsObject:
		arr := make(jsArray, 0, len(t))
		for k, val := range t {
			arr = append(arr, jsArray{k, val})
		}
		return arr
	default:
		return jsArray{}
	}
}

func jsToString(v any) string {
	switch t := v.(type) {
	case jsSentinel:
		return string(t)
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%v", t)
	case jsBigint:
		return fmt.Sprintf("%d", int64(t))
	default:
		return jsDebugString(v)
	}
}
