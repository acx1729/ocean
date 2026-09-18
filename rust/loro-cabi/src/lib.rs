//! C ABI over the Loro CRDT for R1 documents, consumed by the Go package
//! `internal/loro` through cgo.
//!
//! Conventions:
//! - Functions return `0` on success, `1` when they failed without touching the
//!   document, and `2` when they failed after mutating it (the caller must
//!   discard the handle).
//! - The message of the last failure on the calling thread is available through
//!   [`loro_last_error`] until the next call on that thread.
//! - Byte results are returned through [`LoroBuf`] values that the caller frees
//!   with [`loro_buf_free`].
//! - Nothing panics across the boundary: inputs are validated and errors are
//!   reported through status codes. The crate is built with `panic = "abort"`,
//!   so an internal panic would terminate the process rather than unwind.
//!
//! Document layout (shared with the TypeScript client): the root map `meta`
//! and the root tree `blocks`, whose fractional index is enabled with jitter 0
//! on every instance, right after creation and again after each import.

mod ffi;
mod ops;
mod state;
mod validate;

use std::os::raw::c_char;

use loro::{ExportMode, LoroDoc, VersionVector};

pub use ffi::LoroBuf;
use ffi::{finish, Failure};

/// ABI revision reported by [`loro_cabi_version`]; bump on incompatible changes.
pub const ABI_VERSION: u32 = 1;
/// Root map holding the page metadata.
pub(crate) const META: &str = "meta";
/// Root tree holding the blocks.
pub(crate) const BLOCKS: &str = "blocks";

/// An open document. Opaque to C: created by [`loro_doc_new`] and released by
/// [`loro_doc_free`].
pub struct LoroDocHandle {
    doc: LoroDoc,
}

/// Applies the per-instance settings that Loro does not persist.
fn configure(doc: &LoroDoc) {
    doc.get_tree(BLOCKS).enable_fractional_index(0);
}

/// Borrows the document behind a handle.
///
/// # Safety
/// `handle` must be null or a pointer returned by [`loro_doc_new`] that has not
/// been freed.
unsafe fn doc_of<'a>(handle: *mut LoroDocHandle) -> Result<&'a LoroDoc, Failure> {
    // SAFETY: guaranteed by the caller contract above.
    unsafe { handle.as_ref() }
        .map(|h| &h.doc)
        .ok_or_else(|| Failure::Rejected("null document handle".to_string()))
}

/// Borrows a caller-provided byte range.
///
/// # Safety
/// When `len` is non-zero, `ptr` must point to `len` readable bytes that stay
/// valid for the duration of the call.
unsafe fn bytes_of<'a>(ptr: *const u8, len: usize, what: &str) -> Result<&'a [u8], Failure> {
    if len == 0 {
        return Ok(&[]);
    }
    if ptr.is_null() {
        return Err(Failure::Rejected(format!("{what}: null pointer with length {len}")));
    }
    // SAFETY: guaranteed by the caller contract above.
    Ok(unsafe { std::slice::from_raw_parts(ptr, len) })
}

/// Hands `bytes` to the caller through `out`.
///
/// # Safety
/// `out` must be null or a valid, writable `LoroBuf` location.
unsafe fn emit(out: *mut LoroBuf, bytes: Vec<u8>) -> Result<(), Failure> {
    if out.is_null() {
        return Err(Failure::Rejected("null output buffer".to_string()));
    }
    // SAFETY: guaranteed by the caller contract above; LoroBuf has no destructor.
    unsafe { *out = LoroBuf::from_vec(bytes) };
    Ok(())
}

/// The ABI revision of this library.
#[no_mangle]
pub extern "C" fn loro_cabi_version() -> u32 {
    ABI_VERSION
}

/// The message of the last failed call on this thread, or an empty string.
/// Valid until the next call into this library on the same thread.
#[no_mangle]
pub extern "C" fn loro_last_error() -> *const c_char {
    ffi::last_error_ptr()
}

/// Frees a buffer returned by this library. Zeroed buffers are ignored.
///
/// # Safety
/// `buf` must have been returned by this library and not freed before.
#[no_mangle]
pub unsafe extern "C" fn loro_buf_free(buf: LoroBuf) {
    // SAFETY: guaranteed by the caller contract above.
    unsafe { buf.free() }
}

/// Creates an empty document with the R1 instance settings applied.
#[no_mangle]
pub extern "C" fn loro_doc_new() -> *mut LoroDocHandle {
    let doc = LoroDoc::new();
    configure(&doc);
    Box::into_raw(Box::new(LoroDocHandle { doc }))
}

/// Releases a document. Null is ignored.
///
/// # Safety
/// `handle` must be null or a pointer from [`loro_doc_new`] not freed before.
#[no_mangle]
pub unsafe extern "C" fn loro_doc_free(handle: *mut LoroDocHandle) {
    if !handle.is_null() {
        // SAFETY: guaranteed by the caller contract above.
        drop(unsafe { Box::from_raw(handle) });
    }
}

/// Sets the peer id used for changes made through this instance.
///
/// # Safety
/// See [`doc_of`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_set_peer_id(handle: *mut LoroDocHandle, peer: u64) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        doc.set_peer_id(peer)
            .map_err(|e| Failure::Rejected(format!("set peer id: {e}")))
    })
}

/// Imports an update or snapshot. Unparseable bytes are rejected without
/// changing the document.
///
/// # Safety
/// See [`doc_of`] and [`bytes_of`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_import(
    handle: *mut LoroDocHandle,
    ptr: *const u8,
    len: usize,
) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        let bytes = unsafe { bytes_of(ptr, len, "import") }?;
        if bytes.is_empty() {
            return Err(Failure::Rejected("import: empty input".to_string()));
        }
        let outcome = doc.import(bytes);
        // The fractional-index setting lives in the tree state, which a
        // snapshot import may rebuild, so it is re-applied unconditionally.
        configure(doc);
        outcome
            .map(|_| ())
            .map_err(|e| Failure::Rejected(format!("import: {e}")))
    })
}

/// Exports a full snapshot (state plus complete history).
///
/// # Safety
/// See [`doc_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_export_snapshot(
    handle: *mut LoroDocHandle,
    out: *mut LoroBuf,
) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        let bytes = doc
            .export(ExportMode::Snapshot)
            .map_err(|e| Failure::Rejected(format!("export snapshot: {e}")))?;
        unsafe { emit(out, bytes) }
    })
}

/// Exports a shallow snapshot whose history starts at the current version.
///
/// # Safety
/// See [`doc_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_export_shallow_snapshot(
    handle: *mut LoroDocHandle,
    out: *mut LoroBuf,
) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        doc.commit();
        let frontiers = doc.oplog_frontiers();
        let bytes = doc
            .export(ExportMode::shallow_snapshot_owned(frontiers))
            .map_err(|e| Failure::Rejected(format!("export shallow snapshot: {e}")))?;
        unsafe { emit(out, bytes) }
    })
}

/// Exports the updates not covered by the encoded version vector `vv`; an
/// empty `vv` exports the whole history.
///
/// # Safety
/// See [`doc_of`], [`bytes_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_export_updates_from(
    handle: *mut LoroDocHandle,
    vv_ptr: *const u8,
    vv_len: usize,
    out: *mut LoroBuf,
) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        let encoded = unsafe { bytes_of(vv_ptr, vv_len, "version vector") }?;
        let from = if encoded.is_empty() {
            VersionVector::default()
        } else {
            VersionVector::decode(encoded)
                .map_err(|e| Failure::Rejected(format!("decode version vector: {e}")))?
        };
        let bytes = doc
            .export(ExportMode::updates(&from))
            .map_err(|e| Failure::Rejected(format!("export updates: {e}")))?;
        unsafe { emit(out, bytes) }
    })
}

/// Exports the encoded version vector of the document state.
///
/// # Safety
/// See [`doc_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_state_vv(handle: *mut LoroDocHandle, out: *mut LoroBuf) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        unsafe { emit(out, doc.state_vv().encode()) }
    })
}

/// Exports the encoded version vector of the operation log.
///
/// # Safety
/// See [`doc_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_oplog_vv(handle: *mut LoroDocHandle, out: *mut LoroBuf) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        unsafe { emit(out, doc.oplog_vv().encode()) }
    })
}

/// Serializes the whole document state as JSON (see the `state` module).
///
/// # Safety
/// See [`doc_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_state_json(
    handle: *mut LoroDocHandle,
    out: *mut LoroBuf,
) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        let bytes = state::state_json(doc).map_err(Failure::Rejected)?;
        unsafe { emit(out, bytes) }
    })
}

/// Validates and applies a JSON array of ops (see the `ops` module). On
/// success `update_out` receives the Loro update encoding the batch and
/// `result_out` receives `{"created": {"<uuid>": "<tree id>"}, "changed": bool}`.
/// Returns 1 when validation rejects the batch (document untouched) and 2 when
/// a failure happened after the first mutation.
///
/// # Safety
/// See [`doc_of`], [`bytes_of`] and [`emit`].
#[no_mangle]
pub unsafe extern "C" fn loro_doc_apply(
    handle: *mut LoroDocHandle,
    ops_ptr: *const u8,
    ops_len: usize,
    update_out: *mut LoroBuf,
    result_out: *mut LoroBuf,
) -> i32 {
    finish(|| {
        let doc = unsafe { doc_of(handle) }?;
        let ops = unsafe { bytes_of(ops_ptr, ops_len, "ops") }?;
        if update_out.is_null() || result_out.is_null() {
            return Err(Failure::Rejected("null output buffer".to_string()));
        }
        let applied = ops::apply(doc, ops)?;
        unsafe {
            emit(update_out, applied.update)?;
            emit(result_out, applied.result)
        }
    })
}

/// Cheap validation of update or snapshot bytes: parses the blob header and
/// metadata without importing anything or verifying the checksum.
///
/// # Safety
/// See [`bytes_of`].
#[no_mangle]
pub unsafe extern "C" fn loro_update_check_header(ptr: *const u8, len: usize) -> i32 {
    finish(|| {
        let bytes = unsafe { bytes_of(ptr, len, "update") }?;
        if bytes.is_empty() {
            return Err(Failure::Rejected("invalid update: empty input".to_string()));
        }
        LoroDoc::decode_import_blob_meta(bytes, false)
            .map(|_| ())
            .map_err(|e| Failure::Rejected(format!("invalid update: {e}")))
    })
}
