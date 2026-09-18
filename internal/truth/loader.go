package truth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/loro"
)

// Load rebuilds a doc at seq (0 = latest) from its newest covering snapshot
// plus the updates after it. The caller owns the returned Doc.
func (s *Store) Load(ctx context.Context, tx pgx.Tx, workspaceID, docID string, atSeq int64) (*loro.Doc, int64, error) {
	doc := loro.New()
	seq, err := s.catchUp(ctx, tx, doc, workspaceID, docID, 0, atSeq, true)
	if err != nil {
		doc.Close()
		return nil, 0, err
	}
	return doc, seq, nil
}

// catchUp imports everything after fromSeq up to atSeq (0 = latest) into doc.
// When useSnapshot is set and fromSeq is 0 it starts from the newest snapshot.
func (s *Store) catchUp(ctx context.Context, tx pgx.Tx, doc *loro.Doc, workspaceID, docID string, fromSeq, atSeq int64, useSnapshot bool) (int64, error) {
	reached := fromSeq
	if useSnapshot && fromSeq == 0 {
		snap, err := s.LatestSnapshot(ctx, tx, workspaceID, docID, atSeq)
		switch {
		case err == nil:
			if err := doc.Import(snap.Bytes); err != nil {
				return 0, fmt.Errorf("import snapshot %s@%d: %w", docID, snap.Seq, err)
			}
			reached = snap.Seq
		case errors.Is(err, ErrNoSnapshot):
		default:
			return 0, err
		}
	}
	const batch = 500
	for {
		updates, err := s.Updates(ctx, tx, workspaceID, docID, reached, atSeq, batch)
		if err != nil {
			return 0, err
		}
		for _, u := range updates {
			if err := doc.Import(u.Bytes); err != nil {
				// A poisoned row is skipped here and quarantined by the compactor.
				s.log.Warn("skipping unimportable update", "doc", docID, "seq", u.Seq, "error", err)
			}
			reached = u.Seq
		}
		if len(updates) < batch {
			return reached, nil
		}
	}
}
