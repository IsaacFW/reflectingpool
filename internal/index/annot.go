package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/IsaacFW/reflectingpool/internal/scan"
)

// Annot is one row of the annotation cache.
type Annot struct {
	Entry    int64
	State    int
	Prefixes []string
}

// describing are the states that say what an item is. A folder in one of them
// covers everything inside it; a folder that was only skipped does not.
const describing = StateNote | StatePrefix | StateDisplayName

func prefixColumn(prefixes []string) string {
	if len(prefixes) == 0 {
		return ""
	}
	return "|" + strings.Join(prefixes, "|") + "|"
}

func (ix *Index) annotState(ctx context.Context, entry int64) (state int, found bool, err error) {
	err = ix.db.QueryRowContext(ctx, `SELECT state FROM annot WHERE entry = ?`, entry).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return state, err == nil, err
}

// SetAnnot records that an entry is annotated or skipped.
func (ix *Index) SetAnnot(ctx context.Context, a Annot) error {
	ix.qmu.Lock()
	defer ix.qmu.Unlock()
	prev, existed, err := ix.annotState(ctx, a.Entry)
	if err != nil {
		return err
	}
	if _, err := ix.db.ExecContext(ctx, `INSERT OR REPLACE INTO annot VALUES(?,?,?)`, a.Entry, a.State, prefixColumn(a.Prefixes)); err != nil {
		return err
	}
	if !existed {
		ix.reviewedChanged(ctx, a.Entry, +1)
	}
	if was, is := prev&describing != 0, a.State&describing != 0; was != is {
		return ix.coverChanged(ctx, a.Entry, is)
	}
	return nil
}

// ClearAnnot returns an entry to the unreviewed.
func (ix *Index) ClearAnnot(ctx context.Context, entry int64) error {
	ix.qmu.Lock()
	defer ix.qmu.Unlock()
	prev, existed, err := ix.annotState(ctx, entry)
	if err != nil || !existed {
		return err
	}
	if _, err := ix.db.ExecContext(ctx, `DELETE FROM annot WHERE entry = ?`, entry); err != nil {
		return err
	}
	ix.reviewedChanged(ctx, entry, -1)
	if prev&describing != 0 {
		return ix.coverChanged(ctx, entry, false)
	}
	return nil
}

// ReplaceAnnots swaps the whole annotation cache.
func (ix *Index) ReplaceAnnots(ctx context.Context, all []Annot) error {
	ix.qmu.Lock()
	defer ix.qmu.Unlock()
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM annot`); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO annot VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	for _, a := range all {
		if _, err := stmt.ExecContext(ctx, a.Entry, a.State, prefixColumn(a.Prefixes)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, st := range ix.queues {
		st.done, st.cov, st.head = nil, nil, nil
	}
	return ix.fillCovered(ctx)
}

// MarkSkipped records entries as skipped, leaving alone any that already
// carry an annotation, and reports how many were new. The entries are
// unreviewed items of one group of a queue, as PendingInGroup returned them;
// naming the queue lets its count be adjusted in place instead of recounted,
// so the review can carry straight on after skipping tens of thousands.
func (ix *Index) MarkSkipped(ctx context.Context, spec QueueSpec, group []any, ids []int64) (int64, error) {
	raw, err := ix.groupRaw(spec.Groups, group)
	if err != nil {
		return 0, err
	}
	ix.qmu.Lock()
	defer ix.qmu.Unlock()
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`INSERT OR IGNORE INTO annot VALUES(?, %d, '')`, StateSkipped))
	if err != nil {
		return 0, err
	}
	var added int64
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, id)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		added += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	from, group1 := queueKey(spec), lastKey(raw)
	for key, st := range ix.queues {
		st.head = nil
		if key == from && st.done != nil {
			st.done[group1] += added
			continue
		}
		st.done, st.cov = nil, nil // some other view of the same items: count again
	}
	return added, nil
}

// reviewedChanged keeps the counts of every remembered queue right when one
// entry becomes reviewed (+1) or returns to review (-1).
func (ix *Index) reviewedChanged(ctx context.Context, entry, delta int64) {
	for _, st := range ix.queues {
		if delta < 0 {
			st.head = nil // the entry is back in the order somewhere
		}
		if st.done == nil {
			continue
		}
		key, covered, ok, err := ix.groupOf(ctx, st, entry)
		if err != nil {
			st.done, st.cov = nil, nil // count again on the next call
			continue
		}
		if !ok {
			continue
		}
		st.done[key] += delta
		if covered && st.cov != nil {
			st.cov[key] -= delta
		}
	}
}

// coverChanged updates the covered folders after an entry gained or lost a
// description.
func (ix *Index) coverChanged(ctx context.Context, entry int64, described bool) error {
	var kind int
	var share int64
	err := ix.db.QueryRowContext(ctx, `SELECT kind, share FROM entries WHERE id = ?`, entry).Scan(&kind, &share)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// A note on a share says what the share is. It does not stand in for a
	// review of everything in it: if it did, one note would empty the queue.
	if kind != int(scan.KindDir) || share == 0 || share == entry {
		return nil
	}
	if described {
		res, err := ix.db.ExecContext(ctx, `INSERT OR IGNORE INTO covered `+subtreeSQL, entry)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			ix.coveredMoved()
		}
		return nil
	}
	// Removing one folder cannot be done alone: folders beneath it may still
	// be covered by another description.
	return ix.fillCovered(ctx)
}

// fillCovered rebuilds the covered folders from the annotation cache.
func (ix *Index) fillCovered(ctx context.Context) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM covered`); err != nil {
		return err
	}
	fill := fmt.Sprintf(`INSERT INTO covered WITH RECURSIVE sub(id) AS (
			SELECT a.entry FROM annot a JOIN entries e ON e.id = a.entry
			WHERE a.state & %d != 0 AND e.kind = 1 AND e.share != 0 AND e.share != e.id
			UNION
			SELECT d.id FROM entries d INDEXED BY entries_dirs JOIN sub ON d.parent = sub.id WHERE d.kind = 1
		) SELECT id FROM sub`, describing)
	if _, err := tx.ExecContext(ctx, fill); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ix.coveredMoved()
	return nil
}

// coveredMoved discards what queues remember that depends on which folders
// are covered.
func (ix *Index) coveredMoved() {
	for _, st := range ix.queues {
		if st.spec.ExcludeCovered {
			st.cov, st.head = nil, nil
		}
	}
}

// SkippedByShare counts, for each share, the entries that were passed over
// with nothing recorded.
func (ix *Index) SkippedByShare(ctx context.Context) (map[int64]int64, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT e.share, COUNT(*) FROM annot a CROSS JOIN entries e ON e.id = a.entry
		WHERE a.state = ? GROUP BY e.share`, StateSkipped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]int64)
	for rows.Next() {
		var share, n int64
		if err := rows.Scan(&share, &n); err != nil {
			return nil, err
		}
		out[share] = n
	}
	return out, rows.Err()
}
