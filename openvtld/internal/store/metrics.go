package store

// Aggregates for /metrics — derived from the job table at scrape time,
// so counters are correct across restarts without in-memory state.

import "context"

type JobStat struct {
	Kind        string
	Outcome     string // done | failed | cancelled
	Count       int64
	Bytes       int64
	DurationSec int64 // summed wall time of terminal jobs
}

func (s *Store) JobStats(ctx context.Context) ([]JobStat, error) {
	rows, err := s.db.QueryContext(ctx, `
	    SELECT kind, state, COUNT(*),
	           COALESCE(SUM(bytes_done), 0),
	           COALESCE(SUM(strftime('%s', finished_at) - strftime('%s', created_at)), 0)
	    FROM job
	    WHERE state IN ('done', 'failed', 'cancelled')
	    GROUP BY kind, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobStat
	for rows.Next() {
		var j JobStat
		if err := rows.Scan(&j.Kind, &j.Outcome, &j.Count, &j.Bytes, &j.DurationSec); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type CartStateCount struct {
	LibraryID  int // 0 = not yet stamped with an owner
	LocalState string
	Count      int64
}

// CartStateCounts groups carts by (library, local_state) for the gauge.
func (s *Store) CartStateCounts(ctx context.Context) ([]CartStateCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(library_id, 0), local_state, COUNT(*)
		 FROM cartridge GROUP BY library_id, local_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CartStateCount
	for rows.Next() {
		var c CartStateCount
		if err := rows.Scan(&c.LibraryID, &c.LocalState, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
