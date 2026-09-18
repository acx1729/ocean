/*
 * loro_cabi: C ABI over the Loro CRDT for R1 documents.
 *
 * Kept in sync by hand with src/lib.rs. Every fallible function returns a
 * status: LORO_OK, LORO_ERR (nothing changed) or LORO_ERR_PARTIAL (the
 * document may be partially modified; discard the handle). After a failure,
 * loro_last_error() returns the message for the calling thread until the next
 * call into the library on that thread.
 *
 * Buffers returned through LoroBuf are owned by Rust and must be released
 * with loro_buf_free exactly once.
 */
#ifndef LORO_CABI_H
#define LORO_CABI_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

enum {
    LORO_OK = 0,
    LORO_ERR = 1,
    LORO_ERR_PARTIAL = 2
};

/* Opaque document handle. */
typedef struct LoroDocHandle LoroDocHandle;

/* A byte buffer allocated by Rust. `ptr` is only meaningful when `len` > 0. */
typedef struct LoroBuf {
    uint8_t *ptr;
    size_t len;
    size_t cap;
} LoroBuf;

/* ABI revision; the Go binding checks it at start-up. */
uint32_t loro_cabi_version(void);

/* Last error message on this thread (empty string when none). */
const char *loro_last_error(void);

/* Frees a buffer returned by this library. Zeroed buffers are ignored. */
void loro_buf_free(LoroBuf buf);

/* Lifecycle. loro_doc_new enables the fractional index (jitter 0) on the
 * "blocks" tree; loro_doc_free accepts NULL. */
LoroDocHandle *loro_doc_new(void);
void loro_doc_free(LoroDocHandle *doc);

int32_t loro_doc_set_peer_id(LoroDocHandle *doc, uint64_t peer);

/* Imports update or snapshot bytes; re-enables the fractional index. */
int32_t loro_doc_import(LoroDocHandle *doc, const uint8_t *ptr, size_t len);

/* Exports. */
int32_t loro_doc_export_snapshot(LoroDocHandle *doc, LoroBuf *out);
int32_t loro_doc_export_shallow_snapshot(LoroDocHandle *doc, LoroBuf *out);
/* `vv` is an encoded VersionVector; empty means "all updates". */
int32_t loro_doc_export_updates_from(LoroDocHandle *doc, const uint8_t *vv_ptr, size_t vv_len,
                                     LoroBuf *out);
int32_t loro_doc_state_vv(LoroDocHandle *doc, LoroBuf *out);
int32_t loro_doc_oplog_vv(LoroDocHandle *doc, LoroBuf *out);

/* Whole state as JSON: {"meta": {...}, "blocks": [{id, parent, index,
 * fractional_index, meta}, ...]} in tree order, deleted nodes excluded. */
int32_t loro_doc_state_json(LoroDocHandle *doc, LoroBuf *out);

/* Applies a JSON array of ops. On success `update_out` holds the Loro update
 * encoding the batch and `result_out` holds
 * {"created": {"<uuid>": "<tree id>"}, "changed": bool}. */
int32_t loro_doc_apply(LoroDocHandle *doc, const uint8_t *ops_ptr, size_t ops_len,
                       LoroBuf *update_out, LoroBuf *result_out);

/* Header-level validation of update or snapshot bytes. */
int32_t loro_update_check_header(const uint8_t *ptr, size_t len);

#ifdef __cplusplus
}
#endif

#endif /* LORO_CABI_H */
