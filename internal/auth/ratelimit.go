package auth

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
)

// LimitResult is the outcome of a token-bucket check.
type LimitResult struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAfter time.Duration
}

// Limiter is a token bucket keyed by bucket name.
type Limiter interface {
	// Allow takes one token from bucket, which refills to limit tokens every per.
	Allow(ctx context.Context, bucket string, limit int, per time.Duration) (LimitResult, error)
}

// PGLimiter keeps buckets in the rate_limits table so every replica shares them.
type PGLimiter struct {
	db *db.DB
}

// NewPGLimiter returns a database-backed limiter.
func NewPGLimiter(d *db.DB) *PGLimiter { return &PGLimiter{db: d} }

// Allow implements Limiter with one upsert.
func (l *PGLimiter) Allow(ctx context.Context, bucket string, limit int, per time.Duration) (LimitResult, error) {
	if limit <= 0 {
		return LimitResult{Allowed: true, Limit: limit, Remaining: math.MaxInt32}, nil
	}
	rate := float64(limit) / per.Seconds()
	var tokens float64
	err := l.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO rate_limits (bucket, tokens, updated_at) VALUES ($1, $2::float8 - 1, now())
			ON CONFLICT (bucket) DO UPDATE SET
			  tokens = GREATEST(LEAST(rate_limits.tokens + EXTRACT(EPOCH FROM (now() - rate_limits.updated_at)) * $3::float8, $2::float8) - 1, -1),
			  updated_at = now()
			RETURNING tokens`, bucket, float64(limit), rate).Scan(&tokens)
	})
	if err != nil {
		return LimitResult{}, err
	}
	return result(tokens, limit, rate), nil
}

// Purge removes buckets idle for longer than age.
func (l *PGLimiter) Purge(ctx context.Context, age time.Duration) error {
	return l.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM rate_limits WHERE updated_at < now() - $1::interval`, age)
		return err
	})
}

func result(tokens float64, limit int, rate float64) LimitResult {
	r := LimitResult{Allowed: tokens >= 0, Limit: limit}
	if tokens > 0 {
		r.Remaining = int(tokens)
	}
	if !r.Allowed {
		r.ResetAfter = time.Duration(float64(time.Second) / rate)
	}
	return r
}

// MemoryLimiter is a per-process limiter for tests and the desktop profile.
type MemoryLimiter struct {
	mu      sync.Mutex
	buckets map[string]*memBucket
	now     func() time.Time
}

type memBucket struct {
	tokens  float64
	updated time.Time
}

// NewMemoryLimiter returns an in-memory limiter.
func NewMemoryLimiter(clock func() time.Time) *MemoryLimiter {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryLimiter{buckets: map[string]*memBucket{}, now: clock}
}

// Allow implements Limiter.
func (m *MemoryLimiter) Allow(_ context.Context, bucket string, limit int, per time.Duration) (LimitResult, error) {
	if limit <= 0 {
		return LimitResult{Allowed: true, Limit: limit, Remaining: math.MaxInt32}, nil
	}
	rate := float64(limit) / per.Seconds()
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[bucket]
	if !ok {
		b = &memBucket{tokens: float64(limit), updated: now}
		m.buckets[bucket] = b
	}
	b.tokens = math.Min(b.tokens+now.Sub(b.updated).Seconds()*rate, float64(limit))
	b.updated = now
	b.tokens = math.Max(b.tokens-1, -1)
	if len(m.buckets) > 100000 {
		for k, v := range m.buckets {
			if now.Sub(v.updated) > time.Hour {
				delete(m.buckets, k)
			}
		}
	}
	return result(b.tokens, limit, rate), nil
}
