// Package db owns the PostgreSQL connection pools, the embedded schema
// migrations and the transaction helpers that enforce tenant isolation.
//
// Every request-path transaction runs as the kb_app role with app.workspace_id
// and app.principal_id set for the transaction, so the row-level security
// policies from the migrations apply on top of the mandatory workspace_id
// predicate every query carries. System transactions (workers, auth, node
// bootstrap) run as the connection owner and bypass RLS.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options tune the pools.
type Options struct {
	MaxConns int32
	MinConns int32
	AppName  string
	Logger   *slog.Logger
}

// DB holds the primary pool and an optional streaming replica pool.
type DB struct {
	primary *pgxpool.Pool
	replica *pgxpool.Pool
	log     *slog.Logger
}

// Open connects to the primary and, when replicaDSN is not empty, to a replica.
func Open(ctx context.Context, primaryDSN, replicaDSN string, o Options) (*DB, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.AppName == "" {
		o.AppName = "kb"
	}
	open := func(dsn string) (*pgxpool.Pool, error) {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return nil, fmt.Errorf("parse dsn: %w", err)
		}
		if o.MaxConns > 0 {
			cfg.MaxConns = o.MaxConns
		}
		if o.MinConns > 0 {
			cfg.MinConns = o.MinConns
		}
		cfg.ConnConfig.RuntimeParams["application_name"] = o.AppName
		cfg.MaxConnLifetime = time.Hour
		cfg.MaxConnIdleTime = 10 * time.Minute
		cfg.HealthCheckPeriod = 30 * time.Second
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("ping: %w", err)
		}
		return pool, nil
	}
	primary, err := open(primaryDSN)
	if err != nil {
		return nil, fmt.Errorf("primary: %w", err)
	}
	d := &DB{primary: primary, log: o.Logger}
	if replicaDSN != "" {
		replica, err := open(replicaDSN)
		if err != nil {
			primary.Close()
			return nil, fmt.Errorf("replica: %w", err)
		}
		d.replica = replica
	}
	return d, nil
}

// Close closes both pools.
func (d *DB) Close() {
	if d.replica != nil {
		d.replica.Close()
	}
	d.primary.Close()
}

// Primary returns the read-write pool.
func (d *DB) Primary() *pgxpool.Pool { return d.primary }

// Reader returns the replica pool when configured, else the primary.
func (d *DB) Reader() *pgxpool.Pool {
	if d.replica != nil {
		return d.replica
	}
	return d.primary
}

// HasReplica reports whether reads can be routed away from the primary.
func (d *DB) HasReplica() bool { return d.replica != nil }

// Ping checks both pools.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.primary.Ping(ctx); err != nil {
		return fmt.Errorf("primary: %w", err)
	}
	if d.replica != nil {
		if err := d.replica.Ping(ctx); err != nil {
			return fmt.Errorf("replica: %w", err)
		}
	}
	return nil
}

// Tenant scopes a transaction to one workspace and the acting principal.
type Tenant struct {
	WorkspaceID string
	PrincipalID string
}

// TxOptions control a transaction. Zero values mean the spec defaults for the
// application role: statement_timeout 2 s, lock_timeout 1 s and
// idle_in_transaction_session_timeout 10 s.
type TxOptions struct {
	ReadOnly         bool
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	IdleTimeout      time.Duration
	Isolation        pgx.TxIsoLevel
	// Primary forces the primary even for read-only transactions (wait_for_seq).
	Primary bool
}

// Tx runs fn in a transaction scoped to the tenant (nil = system transaction).
func (d *DB) Tx(ctx context.Context, t *Tenant, fn func(context.Context, pgx.Tx) error) error {
	return d.TxWith(ctx, t, TxOptions{}, fn)
}

// ReadTx runs fn in a read-only transaction, on the replica when configured.
func (d *DB) ReadTx(ctx context.Context, t *Tenant, fn func(context.Context, pgx.Tx) error) error {
	return d.TxWith(ctx, t, TxOptions{ReadOnly: true}, fn)
}

// System runs fn as the connection owner, bypassing row-level security.
func (d *DB) System(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return d.TxWith(ctx, nil, TxOptions{}, fn)
}

// TxWith runs fn inside a transaction with explicit options.
func (d *DB) TxWith(ctx context.Context, t *Tenant, o TxOptions, fn func(context.Context, pgx.Tx) error) (err error) {
	pool := d.primary
	if o.ReadOnly && !o.Primary {
		pool = d.Reader()
	}
	txo := pgx.TxOptions{IsoLevel: o.Isolation}
	if o.ReadOnly {
		txo.AccessMode = pgx.ReadOnly
	}
	tx, err := pool.BeginTx(ctx, txo)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if t != nil {
		if err = applyTenant(ctx, tx, t, o); err != nil {
			return err
		}
	} else if o.StatementTimeout > 0 {
		if _, err = tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", millis(o.StatementTimeout)); err != nil {
			return err
		}
	}
	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func applyTenant(ctx context.Context, tx pgx.Tx, t *Tenant, o TxOptions) error {
	st := o.StatementTimeout
	if st == 0 {
		st = 2 * time.Second
	}
	lt := o.LockTimeout
	if lt == 0 {
		lt = time.Second
	}
	it := o.IdleTimeout
	if it == 0 {
		it = 10 * time.Second
	}
	// SET LOCAL ROLE cannot take bind parameters; the role name is a constant.
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE kb_app"); err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	_, err := tx.Exec(ctx, `SELECT set_config('app.workspace_id', $1, true),
	        set_config('app.principal_id', $2, true),
	        set_config('statement_timeout', $3, true),
	        set_config('lock_timeout', $4, true),
	        set_config('idle_in_transaction_session_timeout', $5, true)`,
		t.WorkspaceID, t.PrincipalID, millis(st), millis(lt), millis(it))
	if err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	return nil
}

func millis(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + "ms" }

// Retry runs fn up to attempts times while it fails with a serialization
// failure, deadlock or lock timeout, sleeping briefly between attempts.
func Retry(ctx context.Context, attempts int, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		err = fn()
		if err == nil || !(IsSerializationFailure(err) || IsDeadlock(err) || IsLockTimeout(err)) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(5*(1<<i)) * time.Millisecond):
		}
	}
	return err
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsUniqueViolation reports SQLSTATE 23505.
func IsUniqueViolation(err error) bool { return pgCode(err) == "23505" }

// IsForeignKeyViolation reports SQLSTATE 23503.
func IsForeignKeyViolation(err error) bool { return pgCode(err) == "23503" }

// IsCheckViolation reports SQLSTATE 23514.
func IsCheckViolation(err error) bool { return pgCode(err) == "23514" }

// IsSerializationFailure reports SQLSTATE 40001.
func IsSerializationFailure(err error) bool { return pgCode(err) == "40001" }

// IsDeadlock reports SQLSTATE 40P01.
func IsDeadlock(err error) bool { return pgCode(err) == "40P01" }

// IsLockTimeout reports SQLSTATE 55P03.
func IsLockTimeout(err error) bool { return pgCode(err) == "55P03" }

// IsStatementTimeout reports SQLSTATE 57014 (query canceled).
func IsStatementTimeout(err error) bool { return pgCode(err) == "57014" }

// IsRLSViolation reports SQLSTATE 42501 raised when a write fails a policy.
func IsRLSViolation(err error) bool { return pgCode(err) == "42501" }

// ConstraintName returns the violated constraint, when any.
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// ErrNoRows is re-exported so callers do not import pgx just for it.
var ErrNoRows = pgx.ErrNoRows
