// Package apierr builds Connect errors with the typed details of the error
// model in specification section 12.
package apierr

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
)

func withDetail(err *connect.Error, msg proto.Message) *connect.Error {
	if d, derr := connect.NewErrorDetail(msg); derr == nil {
		err.AddDetail(d)
	}
	return err
}

// Unauthenticated maps a credential failure.
func Unauthenticated(msg string) *connect.Error {
	if msg == "" {
		msg = "authentication required"
	}
	return connect.NewError(connect.CodeUnauthenticated, errors.New(msg))
}

// PermissionDenied carries PermissionDenied{permission, resource, as_owner}.
func PermissionDenied(permission, resource string, asOwner bool) *connect.Error {
	e := connect.NewError(connect.CodePermissionDenied, fmt.Errorf("permission %s denied on %s", permission, resource))
	return withDetail(e, &kbv1.PermissionDenied{Permission: permission, Resource: resource, AsOwner: asOwner})
}

// InvalidArgument carries a BadRequest with one field violation.
func InvalidArgument(field, description string) *connect.Error {
	e := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %s", field, description))
	return withDetail(e, &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: description}}})
}

// InvalidArguments carries several field violations.
func InvalidArguments(violations map[string]string) *connect.Error {
	br := &errdetails.BadRequest{}
	msg := "invalid request"
	for f, d := range violations {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: f, Description: d})
		msg = f + ": " + d
	}
	return withDetail(connect.NewError(connect.CodeInvalidArgument, errors.New(msg)), br)
}

// CelError carries a query compile error with position.
func CelError(line, col int, message, symbol string) *connect.Error {
	e := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("cel %d:%d: %s", line, col, message))
	return withDetail(e, &kbv1.CelError{Line: int32(line), Col: int32(col), Message: message, Symbol: symbol})
}

// NotFound hides whether a resource exists or is merely invisible.
func NotFound(resourceType, id string) *connect.Error {
	e := connect.NewError(connect.CodeNotFound, fmt.Errorf("%s %s not found", resourceType, id))
	return withDetail(e, &errdetails.ResourceInfo{ResourceType: resourceType, ResourceName: id})
}

// VersionConflict carries the current version after an if_version mismatch.
func VersionConflict(current int64) *connect.Error {
	e := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version conflict: current version is %d", current))
	return withDetail(e, &kbv1.VersionConflict{CurrentVersion: current})
}

// SchemaViolation carries missing and invalid properties.
func SchemaViolation(missing []string, invalid map[string]string) *connect.Error {
	sv := &kbv1.SchemaViolation{MissingRequired: missing}
	for id, reason := range invalid {
		sv.Invalid = append(sv.Invalid, &kbv1.SchemaViolation_InvalidProperty{PropertyId: id, Reason: reason})
	}
	e := connect.NewError(connect.CodeFailedPrecondition, errors.New("block violates its type schema"))
	return withDetail(e, sv)
}

// FailedPrecondition is a plain precondition failure.
func FailedPrecondition(msg string) *connect.Error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
}

// ResourceExhausted carries a QuotaFailure.
func ResourceExhausted(subject, description string) *connect.Error {
	e := connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("%s: %s", subject, description))
	return withDetail(e, &errdetails.QuotaFailure{Violations: []*errdetails.QuotaFailure_Violation{{Subject: subject, Description: description}}})
}

// Aborted carries ErrorInfo{reason}.
func Aborted(reason, msg string) *connect.Error {
	e := connect.NewError(connect.CodeAborted, errors.New(msg))
	return withDetail(e, &errdetails.ErrorInfo{Reason: reason, Domain: "kb.v1"})
}

// Unavailable carries RetryInfo.
func Unavailable(msg string, retryAfterMs int64) *connect.Error {
	e := connect.NewError(connect.CodeUnavailable, errors.New(msg))
	if retryAfterMs > 0 {
		return withDetail(e, &errdetails.RetryInfo{RetryDelay: durationpb(retryAfterMs)})
	}
	return e
}

// Internal wraps an unexpected error without leaking details to the client.
func Internal(err error) *connect.Error {
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// Unimplemented marks a method that R1 does not ship.
func Unimplemented(what string) *connect.Error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is not available in this release", what))
}

// AlreadyExists reports a uniqueness conflict.
func AlreadyExists(what string) *connect.Error {
	return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", what))
}

// From returns err unchanged when it already is a Connect error, else Internal.
func From(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return Internal(err)
}
