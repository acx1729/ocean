// Package api implements the kb.v1 services over the truth layer: workspaces,
// projects, members, schema, pages and blocks. Every handler resolves the
// caller, asks the Guard, runs inside a tenant transaction and maps errors to
// the section 12 error model.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/audit"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/keyring"
	"github.com/acx1729/ocean/internal/schema"
	"github.com/acx1729/ocean/internal/truth"
)

// Provisioner creates the authorization store of a new workspace. The
// OpenFGA-backed implementation arrives with the authorization milestone; the
// default records placeholders.
type Provisioner interface {
	ProvisionWorkspace(ctx context.Context, tx pgx.Tx, workspaceID, adminDID string) (storeID, modelID string, err error)
	ProvisionProject(ctx context.Context, tx pgx.Tx, workspaceID, projectID string) error
	ProvisionDoc(ctx context.Context, tx pgx.Tx, workspaceID, projectID, docID, parentDocID string) error
}

// NoopProvisioner is used until OpenFGA is wired.
type NoopProvisioner struct{}

// ProvisionWorkspace implements Provisioner.
func (NoopProvisioner) ProvisionWorkspace(context.Context, pgx.Tx, string, string) (string, string, error) {
	return "pending", "pending", nil
}

// ProvisionProject implements Provisioner.
func (NoopProvisioner) ProvisionProject(context.Context, pgx.Tx, string, string) error { return nil }

// ProvisionDoc implements Provisioner.
func (NoopProvisioner) ProvisionDoc(context.Context, pgx.Tx, string, string, string, string) error {
	return nil
}

// Deps are the collaborators of the services.
type Deps struct {
	Config    *config.Config
	DB        *db.DB
	Keys      *keyring.KeyRing
	Truth     *truth.Store
	Mat       *truth.Materializer
	Guard     authz.Guard
	AuthStore *auth.Store
	Schema    schema.Repo
	Splitter  Splitter
	Provision Provisioner
	Log       *slog.Logger
	Clock     func() time.Time
	// Notify is called after a commit with the plaintext update of a changed
	// doc, so sync rooms can fan it out. Optional.
	Notify func(workspaceID, docID string, seq int64, update []byte, actor string)
}

// Services bundles the handlers.
type Services struct {
	Workspaces *WorkspacesService
	Projects   *ProjectsService
	Members    *MembersService
	Schema     *SchemaService
	Pages      *PagesService
	Blocks     *BlocksService
}

// New wires the services.
func New(d Deps) *Services {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Splitter == nil {
		d.Splitter = NaiveSplitter{}
	}
	if d.Provision == nil {
		d.Provision = NoopProvisioner{}
	}
	c := &core{Deps: d}
	return &Services{
		Workspaces: &WorkspacesService{c},
		Projects:   &ProjectsService{c},
		Members:    &MembersService{c},
		Schema:     &SchemaService{c},
		Pages:      &PagesService{c},
		Blocks:     &BlocksService{c},
	}
}

type core struct {
	Deps
}

// caller returns the authenticated identity or an Unauthenticated error.
func (c *core) caller(ctx context.Context) (*auth.Identity, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, apierr.Unauthenticated("")
	}
	if id.Operator {
		return nil, apierr.PermissionDenied("api", "operator token is limited to AdminService", false)
	}
	return id, nil
}

func tenant(id *auth.Identity, ws string) *db.Tenant {
	return &db.Tenant{WorkspaceID: ws, PrincipalID: id.EffectiveOwner()}
}

// check runs a guard check and returns its Connect error.
func (c *core) check(ctx context.Context, id *auth.Identity, ws string, perm authz.Permission, res authz.Resource) error {
	if ws == "" {
		return apierr.InvalidArgument("workspace_id", "required")
	}
	if _, err := uuid.Parse(ws); err != nil {
		return apierr.NotFound("workspace", ws)
	}
	return c.Guard.Check(ctx, id, ws, perm, res)
}

// audit appends an audit entry inside tx; failures are logged, never fatal.
func (c *core) audit(ctx context.Context, tx pgx.Tx, id *auth.Identity, ws, action, resourceType, resourceID string, detail map[string]any) {
	e := audit.Entry{Principal: id.Principal, Owner: id.EffectiveOwner(), Action: action, ResourceType: resourceType, ResourceID: resourceID, Detail: detail}
	if err := audit.Append(ctx, tx, ws, e, c.Clock()); err != nil {
		c.Log.Error("audit append", "error", err, "action", action)
	}
}

// mapErr converts domain errors to Connect errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	var vc *truth.VersionConflictError
	switch {
	case errors.As(err, &vc):
		return apierr.VersionConflict(vc.Current)
	case truth.IsNotFound(err) || errors.Is(err, schema.ErrNotFound):
		return apierr.NotFound("resource", "")
	case errors.Is(err, truth.ErrDeleted):
		return apierr.FailedPrecondition("the page is in the trash")
	case errors.Is(err, schema.ErrInUse):
		return apierr.FailedPrecondition("the type is still used by blocks or subtypes")
	case db.IsUniqueViolation(err):
		return apierr.AlreadyExists("resource")
	case db.IsStatementTimeout(err):
		return apierr.ResourceExhausted("statement", "query timed out")
	case db.IsRLSViolation(err):
		return apierr.NotFound("resource", "")
	}
	slog.Error("internal error", "error", err)
	return apierr.Internal(err)
}

// --- idempotency -----------------------------------------------------------

// idempotencyKey reads and validates the Idempotency-Key header.
func idempotencyKey(req connect.AnyRequest) (string, error) {
	k := strings.TrimSpace(req.Header().Get("Idempotency-Key"))
	if k == "" {
		return "", nil
	}
	if _, err := uuid.Parse(k); err != nil {
		return "", apierr.InvalidArgument("Idempotency-Key", "must be a UUID")
	}
	return k, nil
}

func requestHash(msg proto.Message) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

// idempotent runs fn unless the same key was already used with the same
// request, in which case the stored response is returned. A different request
// under the same key is ABORTED. The response is stored inside the mutation
// transaction through the returned recorder.
func idempotent[Req any, Res proto.Message](ctx context.Context, c *core, id *auth.Identity, ws string, req *connect.Request[Req], newRes func() Res, fn func(record func(ctx context.Context, tx pgx.Tx, res Res) error) (Res, error)) (Res, error) {
	var zero Res
	key, err := idempotencyKey(req)
	if err != nil {
		return zero, err
	}
	if key == "" {
		return fn(func(context.Context, pgx.Tx, Res) error { return nil })
	}
	msg, ok := any(req.Msg).(proto.Message)
	if !ok {
		return zero, apierr.Internal(errors.New("request is not a proto message"))
	}
	hash, err := requestHash(msg)
	if err != nil {
		return zero, apierr.Internal(err)
	}
	var stored []byte
	var storedHash []byte
	err = c.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT request_hash, response FROM idempotency_keys WHERE workspace_id = $1 AND principal_id = $2 AND key = $3`, ws, id.Principal, key).Scan(&storedHash, &stored)
	})
	switch {
	case err == nil:
		if string(storedHash) != string(hash) {
			return zero, apierr.Aborted("IDEMPOTENCY_MISMATCH", "Idempotency-Key was used with a different request")
		}
		res := newRes()
		if err := proto.Unmarshal(stored, res); err != nil {
			return zero, apierr.Internal(err)
		}
		return res, nil
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return zero, mapErr(err)
	}
	return fn(func(ctx context.Context, tx pgx.Tx, res Res) error {
		b, err := proto.Marshal(res)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys (workspace_id, principal_id, key, request_hash, response) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (workspace_id, principal_id, key) DO NOTHING`, ws, id.Principal, key, hash, b)
		return err
	})
}

// --- cursors -----------------------------------------------------------------

func encodeCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x1f")))
}

func decodeCursor(s string, n int) ([]string, error) {
	if s == "" {
		return make([]string, n), nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, apierr.InvalidArgument("cursor", "invalid")
	}
	parts := strings.Split(string(b), "\x1f")
	if len(parts) != n {
		return nil, apierr.InvalidArgument("cursor", "invalid")
	}
	return parts, nil
}

func pageSize(n int32) int {
	switch {
	case n <= 0:
		return 50
	case n > 500:
		return 500
	}
	return int(n)
}

// --- conversions -------------------------------------------------------------

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

// toStruct converts a property bag for the wire: LoroMap<id,true> sets become
// sorted lists.
func toStruct(m map[string]any) *structpb.Struct {
	out := map[string]any{}
	for k, v := range m {
		out[k] = wireValue(v)
	}
	s, err := structpb.NewStruct(out)
	if err != nil {
		s, _ = structpb.NewStruct(map[string]any{})
	}
	return s
}

func wireValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		allTrue := len(x) > 0
		for _, val := range x {
			if b, ok := val.(bool); !ok || !b {
				allTrue = false
				break
			}
		}
		if allTrue {
			keys := make([]any, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sortAny(keys)
			return keys
		}
		out := map[string]any{}
		for k, val := range x {
			out[k] = wireValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = wireValue(val)
		}
		return out
	}
	return v
}

func sortAny(xs []any) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && fmt.Sprint(xs[j-1]) > fmt.Sprint(xs[j]); j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func fromStruct(s *structpb.Struct) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	return s.AsMap()
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func (c *core) uri(ws, blockID string) string { return "kb://" + ws + "/" + blockID }

// parseURI extracts the block id from kb://{workspace}/{block}.
func parseURI(ws, u string) (string, bool) {
	rest, ok := strings.CutPrefix(u, "kb://")
	if !ok {
		return "", false
	}
	w, id, ok := strings.Cut(rest, "/")
	if !ok || w != ws || !validUUID(id) {
		return "", false
	}
	return id, true
}
