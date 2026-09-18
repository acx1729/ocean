package loro

import "errors"

// Status codes reported by the C ABI.
const (
	// CodeError means the call failed and the document is unchanged.
	CodeError = 1
	// CodePartial means the call failed after the document was mutated.
	CodePartial = 2
)

// ErrPartial is wrapped by every Error with Code == CodePartial: a batch
// failed after its first mutation, so the Doc is in an undefined state and
// must be discarded (Close it and reload from the log).
var ErrPartial = errors.New("loro: document may be partially modified, discard it")

// ErrClosed is returned by every method of a Doc that has been closed.
var ErrClosed = errors.New("loro: document is closed")

// Error is a failure reported by the Loro C ABI.
type Error struct {
	// Code is CodeError or CodePartial.
	Code int
	// Msg is the message recorded by the library, e.g.
	// "op[2] tree.move: node 5@17 does not exist or is deleted".
	Msg string
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Code == CodePartial {
		return "loro: " + e.Msg + " (document partially modified)"
	}
	return "loro: " + e.Msg
}

// Unwrap makes errors.Is(err, ErrPartial) true for partial failures.
func (e *Error) Unwrap() error {
	if e.Code == CodePartial {
		return ErrPartial
	}
	return nil
}
