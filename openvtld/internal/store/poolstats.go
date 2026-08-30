package store

// dedupe_stats — persisted pool samples so the dashboard capacity trend
// survives restarts (v0.4's chart was session-only). The pool column is
// the pool name; multi-pool since v0.6.

import (
	"context"
	"time"
)

type PoolSample struct {
	TS           string  `json:"ts"`
	Pool         string  `json:"pool,omitempty"`
	FSUsedBytes  int64   `json:"fs_used_bytes"`
	FSTotalBytes int64   `json:"fs_total_bytes"`
	VDOUsedBytes int64   `json:"vdo_used_bytes"` // physical (zfs `used`, compressed)
	VDOPhysBytes int64   `json:"vdo_phys_bytes"`
	VDOSavingPct int64   `json:"vdo_saving_pct"`
	LogicalBytes int64   `json:"logical_bytes"` // pre-dedup/compress (zfs `logicalused`)
	CacheUsedPct float64 `json:"cache_used_pct"`
}

func (s *Store) AddPoolSample(ctx context.Context, p PoolSample) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO dedupe_stats
		 (ts, pool, fs_used_bytes, fs_total_bytes, vdo_used_bytes, vdo_phys_bytes, vdo_saving_pct, logical_bytes, cache_used_pct)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.TS, p.Pool, p.FSUsedBytes, p.FSTotalBytes, p.VDOUsedBytes, p.VDOPhysBytes, p.VDOSavingPct, p.LogicalBytes, p.CacheUsedPct)
	return err
}

// LastPoolSample returns the most recent sample for change detection
// (the collector skips writes when nothing moved).
func (s *Store) LastPoolSample(ctx context.Context, pool string) (*PoolSample, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT ts, pool, fs_used_bytes, fs_total_bytes, vdo_used_bytes, vdo_phys_bytes, vdo_saving_pct, logical_bytes, cache_used_pct
		 FROM dedupe_stats WHERE pool = ? ORDER BY ts DESC LIMIT 1`, pool)
	var p PoolSample
	if err := row.Scan(&p.TS, &p.Pool, &p.FSUsedBytes, &p.FSTotalBytes, &p.VDOUsedBytes, &p.VDOPhysBytes, &p.VDOSavingPct, &p.LogicalBytes, &p.CacheUsedPct); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) PoolHistory(ctx context.Context, pool string, since time.Time) ([]PoolSample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, pool, fs_used_bytes, fs_total_bytes, vdo_used_bytes, vdo_phys_bytes, vdo_saving_pct, logical_bytes, cache_used_pct
		 FROM dedupe_stats WHERE pool = ? AND ts >= ? ORDER BY ts`,
		pool, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolSample
	for rows.Next() {
		var p PoolSample
		if err := rows.Scan(&p.TS, &p.Pool, &p.FSUsedBytes, &p.FSTotalBytes, &p.VDOUsedBytes, &p.VDOPhysBytes, &p.VDOSavingPct, &p.LogicalBytes, &p.CacheUsedPct); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PrunePoolSamples drops samples older than the retention window.
func (s *Store) PrunePoolSamples(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM dedupe_stats WHERE ts < ?`, olderThan.UTC().Format(time.RFC3339))
	return err
}
