//! Plumbing shared by the exported functions: byte buffers, status codes and
//! the per-thread error slot.

use std::cell::RefCell;
use std::ffi::CString;
use std::mem::ManuallyDrop;
use std::os::raw::c_char;

/// The call succeeded.
pub const STATUS_OK: i32 = 0;
/// The call failed and the document is untouched.
pub const STATUS_ERROR: i32 = 1;
/// The call failed after the document was mutated; the caller must discard it.
pub const STATUS_PARTIAL: i32 = 2;

/// A heap buffer handed to the caller. Its allocation belongs to Rust and must
/// be returned through `loro_buf_free` exactly once.
#[repr(C)]
pub struct LoroBuf {
    /// Start of the data. Only meaningful when `len` is non-zero.
    pub ptr: *mut u8,
    /// Number of valid bytes.
    pub len: usize,
    /// Capacity of the allocation; needed to free it.
    pub cap: usize,
}

impl LoroBuf {
    /// Moves a vector's allocation into a buffer without copying.
    pub fn from_vec(bytes: Vec<u8>) -> LoroBuf {
        let mut bytes = ManuallyDrop::new(bytes);
        LoroBuf {
            ptr: bytes.as_mut_ptr(),
            len: bytes.len(),
            cap: bytes.capacity(),
        }
    }

    /// Releases the allocation.
    ///
    /// # Safety
    /// `self` must have been produced by [`LoroBuf::from_vec`] and must not be
    /// used afterwards. Zeroed or empty buffers are accepted and ignored.
    pub unsafe fn free(self) {
        if self.ptr.is_null() || self.cap == 0 {
            return;
        }
        // SAFETY: the fields describe a Vec<u8> allocation produced by from_vec.
        drop(unsafe { Vec::from_raw_parts(self.ptr, self.len, self.cap) });
    }
}

/// Why an exported function failed.
#[derive(Debug)]
pub enum Failure {
    /// Nothing was changed; maps to [`STATUS_ERROR`].
    Rejected(String),
    /// A mutation had already been issued; maps to [`STATUS_PARTIAL`].
    Partial(String),
}

impl Failure {
    /// The status code reported to the caller.
    pub fn status(&self) -> i32 {
        match self {
            Failure::Rejected(_) => STATUS_ERROR,
            Failure::Partial(_) => STATUS_PARTIAL,
        }
    }

    /// The human-readable message.
    pub fn message(&self) -> &str {
        match self {
            Failure::Rejected(m) | Failure::Partial(m) => m,
        }
    }
}

impl From<String> for Failure {
    fn from(message: String) -> Self {
        Failure::Rejected(message)
    }
}

impl From<&str> for Failure {
    fn from(message: &str) -> Self {
        Failure::Rejected(message.to_string())
    }
}

thread_local! {
    static LAST_ERROR: RefCell<Option<CString>> = const { RefCell::new(None) };
}

/// A NUL-terminated empty string returned when no error is recorded.
static NO_ERROR: &[u8] = b"\0";

/// Records `message` as the calling thread's last error.
pub fn set_last_error(message: &str) {
    let sanitized = message.replace('\0', "\\0");
    let c = CString::new(sanitized).expect("interior NUL bytes were removed");
    LAST_ERROR.with(|slot| *slot.borrow_mut() = Some(c));
}

/// Forgets the calling thread's last error.
pub fn clear_last_error() {
    LAST_ERROR.with(|slot| *slot.borrow_mut() = None);
}

/// Pointer to the calling thread's last error message, or to an empty string.
/// The pointer stays valid until the next exported call on this thread.
pub fn last_error_ptr() -> *const c_char {
    LAST_ERROR.with(|slot| {
        slot.borrow()
            .as_ref()
            .map_or(NO_ERROR.as_ptr().cast::<c_char>(), |c| c.as_ptr())
    })
}

/// Runs an exported function body and translates the outcome into a status
/// code, recording or clearing the last error accordingly.
pub fn finish(body: impl FnOnce() -> Result<(), Failure>) -> i32 {
    match body() {
        Ok(()) => {
            clear_last_error();
            STATUS_OK
        }
        Err(failure) => {
            set_last_error(failure.message());
            failure.status()
        }
    }
}
