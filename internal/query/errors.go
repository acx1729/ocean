package query

import (
	"errors"
	"fmt"
	"regexp"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
)

// Sentinel errors returned by Compile, Program and the cursor helpers. They
// may be wrapped in a *CelError; use errors.Is to test for them.
var (
	// ErrCursorExpired is returned when a cursor was produced under a
	// different schema version or order_by than the current request.
	ErrCursorExpired = errors.New("query: cursor expired")
	// ErrCostExceeded is returned when the cost estimate of an expression
	// exceeds the limit of 50 units, or when the expression exceeds the AST
	// node cap.
	ErrCostExceeded = errors.New("query: cost limit exceeded")
	// ErrUnsupported is returned for syntax that type-checks but has no SQL
	// translation in R1 (map, filter, size() on text, arithmetic on numbers,
	// conditionals, ...).
	ErrUnsupported = errors.New("query: unsupported expression")
	// errInvalidCursor is wrapped for cursors that cannot be decoded.
	errInvalidCursor = errors.New("query: invalid cursor")
)

// CelError is a positioned compile error. Line and Col are 1-based; Symbol
// names the offending identifier, property, type, option or relation when
// one can be attributed.
type CelError struct {
	Line    int
	Col     int
	Message string
	Symbol  string
	cause   error
}

// Error implements error.
func (e *CelError) Error() string {
	if e.Symbol != "" {
		return fmt.Sprintf("%d:%d: %s (symbol %q)", e.Line, e.Col, e.Message, e.Symbol)
	}
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Message)
}

// Unwrap exposes the sentinel error (ErrUnsupported, ErrCostExceeded, ...)
// that classifies this error, if any.
func (e *CelError) Unwrap() error { return e.cause }

// errorList bundles several compile errors into one error value.
type errorList []*CelError

func (l errorList) Error() string {
	if len(l) == 1 {
		return l[0].Error()
	}
	return fmt.Sprintf("%s (and %d more errors)", l[0].Error(), len(l)-1)
}

func (l errorList) Unwrap() []error {
	out := make([]error, len(l))
	for i, e := range l {
		out[i] = e
	}
	return out
}

// asError converts a list of compile errors into a single error value.
func (l errorList) asError() error {
	switch len(l) {
	case 0:
		return nil
	case 1:
		return l[0]
	default:
		return l
	}
}

var (
	reUndeclared     = regexp.MustCompile(`undeclared reference to '([^']+)'`)
	reUndefinedField = regexp.MustCompile(`undefined field '([^']+)'`)
)

// issuesToErrors converts cel-go parse or check issues into CelErrors.
func issuesToErrors(iss *cel.Issues) errorList {
	if iss == nil || iss.Err() == nil {
		return nil
	}
	var out errorList
	for _, e := range iss.Errors() {
		ce := &CelError{Message: e.Message}
		if e.Location != nil {
			ce.Line = e.Location.Line()
			ce.Col = e.Location.Column() + 1
		}
		if m := reUndeclared.FindStringSubmatch(e.Message); m != nil {
			ce.Symbol = m[1]
			ce.Message = fmt.Sprintf("unknown symbol %q", m[1])
		} else if m := reUndefinedField.FindStringSubmatch(e.Message); m != nil {
			ce.Symbol = "props." + m[1]
			ce.Message = fmt.Sprintf("unknown property %q", m[1])
		}
		out = append(out, ce)
	}
	if len(out) == 0 {
		out = append(out, &CelError{Line: 1, Col: 1, Message: iss.Err().Error()})
	}
	return out
}

// errAt builds a CelError positioned at the given expression id.
func errAt(a *ast.AST, id int64, cause error, symbol string, format string, args ...any) *CelError {
	ce := &CelError{Message: fmt.Sprintf(format, args...), Symbol: symbol, cause: cause}
	if a != nil {
		if loc := a.SourceInfo().GetStartLocation(id); loc != nil {
			ce.Line = loc.Line()
			ce.Col = loc.Column() + 1
		}
	}
	if ce.Line == 0 {
		ce.Line, ce.Col = 1, 1
	}
	return ce
}
