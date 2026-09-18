// Package loro is the Go binding to the Loro CRDT that stores R1 documents.
//
// It links the loro_cabi static library built from rust/loro-cabi (run
// rust/loro-cabi/build.sh first; the library is produced in the git-ignored
// target/release directory) and calls it through cgo.
//
// # Document layout
//
// A document has two root containers, shared with the TypeScript client built
// on loro-crdt: the "meta" LoroMap (title, icon, format, journal_date,
// type_id and the nested "props" LoroMap) and the "blocks" LoroTree. Each tree
// node's data map holds the block id ("id"), "type_id", the nested "props"
// LoroMap, the nested "content" LoroText with the block's Markdown source and,
// when set, "portal_doc_id", "created_at" and "created_by". Multi-valued
// relation properties are nested LoroMaps of target id to true so concurrent
// additions merge. The tree's fractional index is enabled with jitter 0 on
// every instance, so sibling order is stable across peers. Tree ids
// ("<counter>@<peer>") are Loro's node identities and address blocks in ops;
// block ids are the UUIDs stored in the node data.
//
// # Writing
//
// Writes go through Doc.Apply with a batch of ops built by the constructors of
// this package (MetaSet, TreeCreate, NodeText, ...). A batch is validated
// completely before anything is mutated, so a rejected batch leaves the
// document untouched. On success Apply returns the Loro update that encodes
// exactly the batch, ready to be appended to the document log and fanned out.
//
// # Concurrency and memory
//
// A Doc is not safe for concurrent use; callers serialize access to each Doc.
// Distinct Docs may be used from different goroutines. Close releases the
// Rust-side document; a finalizer is registered as a backstop, but callers
// should Close explicitly because the memory held by a document is invisible
// to the Go garbage collector. Every byte slice returned by this package is a
// Go copy; nothing returned refers to Rust memory.
package loro

/*
#cgo CFLAGS: -I${SRCDIR}/../../rust/loro-cabi
#cgo LDFLAGS: ${SRCDIR}/../../rust/loro-cabi/target/release/libloro_cabi.a -lm -ldl -lpthread
#include <stddef.h>
#include <stdint.h>
#include "loro_cabi.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime"
	"unsafe"
)

// abiVersion is the C ABI revision this package is written against.
const abiVersion = 1

func init() {
	if v := Version(); v != abiVersion {
		panic(fmt.Sprintf("loro: libloro_cabi reports ABI version %d, this package expects %d; rebuild rust/loro-cabi", v, abiVersion))
	}
}

// Version returns the ABI revision of the linked loro_cabi library.
func Version() uint32 {
	return uint32(C.loro_cabi_version())
}

// Doc is an open Loro document. It is not safe for concurrent use.
type Doc struct {
	h *C.LoroDocHandle
}

// New creates an empty document.
func New() *Doc {
	h := C.loro_doc_new()
	if h == nil {
		panic("loro: loro_doc_new returned NULL")
	}
	d := &Doc{h: h}
	runtime.SetFinalizer(d, (*Doc).Close)
	return d
}

// FromBytes creates a document and imports each part (snapshots or updates)
// in order. On failure the partially built document is closed.
func FromBytes(parts ...[]byte) (*Doc, error) {
	d := New()
	for _, part := range parts {
		if err := d.Import(part); err != nil {
			d.Close()
			return nil, err
		}
	}
	return d, nil
}

// Close releases the document. It is idempotent; every later method call
// returns ErrClosed.
func (d *Doc) Close() {
	if d.h == nil {
		return
	}
	C.loro_doc_free(d.h)
	d.h = nil
	runtime.SetFinalizer(d, nil)
}

// call runs one C entry point. The goroutine is pinned to its OS thread for
// the duration so that the thread-local error message read after a failure
// belongs to this call.
func (d *Doc) call(fn func(h *C.LoroDocHandle) C.int32_t) error {
	if d.h == nil {
		return ErrClosed
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err := statusError(fn(d.h))
	runtime.KeepAlive(d)
	return err
}

// statusError converts a status code into an error, reading the thread-local
// message for failures. It must run on the thread that made the call.
func statusError(code C.int32_t) error {
	if code == C.LORO_OK {
		return nil
	}
	return &Error{Code: int(code), Msg: C.GoString(C.loro_last_error())}
}

// bytesArg returns the pointer and length of b for a C call. Empty input is
// passed as a null pointer with length 0, which the library accepts.
func bytesArg(b []byte) (*C.uint8_t, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(&b[0])), C.size_t(len(b))
}

// takeBuf copies a buffer returned by the library into Go memory and frees
// the Rust allocation.
func takeBuf(buf *C.LoroBuf) []byte {
	out := make([]byte, int(buf.len))
	if buf.len > 0 && buf.ptr != nil {
		copy(out, unsafe.Slice((*byte)(unsafe.Pointer(buf.ptr)), int(buf.len)))
	}
	C.loro_buf_free(*buf)
	*buf = C.LoroBuf{}
	return out
}

// exportWith runs an entry point that fills one output buffer.
func (d *Doc) exportWith(fn func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t) ([]byte, error) {
	var buf C.LoroBuf
	if err := d.call(func(h *C.LoroDocHandle) C.int32_t { return fn(h, &buf) }); err != nil {
		return nil, err
	}
	return takeBuf(&buf), nil
}

// Import applies update or snapshot bytes. Bytes whose dependencies are
// missing are kept pending by Loro and applied once the missing updates
// arrive. Unparseable input is rejected without changing the document.
func (d *Doc) Import(b []byte) error {
	return d.call(func(h *C.LoroDocHandle) C.int32_t {
		p, n := bytesArg(b)
		return C.loro_doc_import(h, p, n)
	})
}

// ImportAll imports each part in order and stops at the first failure; the
// parts imported before it stay applied.
func (d *Doc) ImportAll(parts [][]byte) error {
	for _, part := range parts {
		if err := d.Import(part); err != nil {
			return err
		}
	}
	return nil
}

// ExportSnapshot exports the state together with the complete history.
func (d *Doc) ExportSnapshot() ([]byte, error) {
	return d.exportWith(func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t {
		return C.loro_doc_export_snapshot(h, out)
	})
}

// ExportShallowSnapshot exports the state with the history truncated at the
// current version; it is the compaction format.
func (d *Doc) ExportShallowSnapshot() ([]byte, error) {
	return d.exportWith(func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t {
		return C.loro_doc_export_shallow_snapshot(h, out)
	})
}

// ExportUpdatesFrom exports the updates not covered by the encoded version
// vector vv (as returned by StateVV or OplogVV of the receiving document).
// A nil or empty vv exports the whole history.
func (d *Doc) ExportUpdatesFrom(vv []byte) ([]byte, error) {
	return d.exportWith(func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t {
		p, n := bytesArg(vv)
		return C.loro_doc_export_updates_from(h, p, n, out)
	})
}

// StateVV returns the encoded version vector of the document state.
func (d *Doc) StateVV() ([]byte, error) {
	return d.exportWith(func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t {
		return C.loro_doc_state_vv(h, out)
	})
}

// OplogVV returns the encoded version vector of the operation log, which also
// covers updates that are pending because their dependencies are missing.
func (d *Doc) OplogVV() ([]byte, error) {
	return d.exportWith(func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t {
		return C.loro_doc_oplog_vv(h, out)
	})
}

// SetPeerID sets the peer id stamped on changes made through this Doc. Peer
// ids must be unique among concurrent writers of a document.
func (d *Doc) SetPeerID(id uint64) error {
	return d.call(func(h *C.LoroDocHandle) C.int32_t {
		return C.loro_doc_set_peer_id(h, C.uint64_t(id))
	})
}

// ApplyResult is the outcome of a successful Apply.
type ApplyResult struct {
	// Update is the Loro update encoding exactly this batch (an update with
	// no changes when Changed is false).
	Update []byte
	// Created maps the block id of every TreeCreate op to the tree id of the
	// created node.
	Created map[string]string
	// Changed reports whether the batch produced any operation; setting a key
	// to the value it already has, for example, produces none.
	Changed bool
}

// Apply validates and applies a batch of ops as one Loro transaction.
//
// Every op is validated before anything is mutated: referenced nodes must
// exist and not be deleted, parents must exist, indexes must be in range,
// keys reserved for dedicated ops are rejected and moves may not create
// cycles. A rejected batch returns an *Error with Code CodeError and leaves
// the document untouched. An error wrapping ErrPartial means an internal
// failure happened after the first mutation; the Doc must then be discarded.
func (d *Doc) Apply(ops []Op) (*ApplyResult, error) {
	if d.h == nil {
		return nil, ErrClosed
	}
	if ops == nil {
		ops = []Op{}
	}
	payload, err := json.Marshal(ops)
	if err != nil {
		return nil, &Error{Code: CodeError, Msg: fmt.Sprintf("encode ops: %v", err)}
	}
	var update, result C.LoroBuf
	err = d.call(func(h *C.LoroDocHandle) C.int32_t {
		p, n := bytesArg(payload)
		return C.loro_doc_apply(h, p, n, &update, &result)
	})
	if err != nil {
		return nil, err
	}
	res := &ApplyResult{Update: takeBuf(&update)}
	var parsed struct {
		Created map[string]string `json:"created"`
		Changed bool              `json:"changed"`
	}
	if err := json.Unmarshal(takeBuf(&result), &parsed); err != nil {
		return nil, &Error{Code: CodePartial, Msg: fmt.Sprintf("decode apply result: %v", err)}
	}
	res.Created = parsed.Created
	res.Changed = parsed.Changed
	return res, nil
}

// stateJSON returns the raw state document produced by the library.
func (d *Doc) stateJSON() ([]byte, error) {
	return d.exportWith(func(h *C.LoroDocHandle, out *C.LoroBuf) C.int32_t {
		return C.loro_doc_state_json(h, out)
	})
}

// State returns a parsed copy of the whole document state.
func (d *Doc) State() (*State, error) {
	raw, err := d.stateJSON()
	if err != nil {
		return nil, err
	}
	return parseState(raw)
}

// CheckUpdateHeader validates the header and metadata of update or snapshot
// bytes without importing them. It is the cheap check a sync room runs on
// pushed updates; it does not verify the checksum or the body, which a later
// Import does.
func CheckUpdateHeader(b []byte) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p, n := bytesArg(b)
	return statusError(C.loro_update_check_header(p, n))
}
