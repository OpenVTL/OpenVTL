package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Job rows drive the export/import/evict state machines. Every state
// transition lands in job_event; the pipeline resumes interrupted jobs
// from (state, export_chunk ledger) after a daemon restart.
type Job struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"` // export | import | evict
	CartLabel   string `json:"cart_label"`
	RemoteID    *int64 `json:"remote_id,omitempty"`
	Generation  string `json:"generation,omitempty"`
	State       string `json:"state"`
	Trigger     string `json:"trigger"` // manual | ie-watcher | policy
	BytesTotal  int64  `json:"bytes_total"`
	BytesDone   int64  `json:"bytes_done"`
	ChunksTotal int    `json:"chunks_total"`
	ChunksDone  int    `json:"chunks_done"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	// Import-only (Phase A cross-instance import): which system's manifest
	// to pull, and the local library the cart is slotted into. Nil for
	// same-system re-imports and for export/evict.
	SystemName    string `json:"system_name,omitempty"`
	TargetLibrary *int64 `json:"target_library,omitempty"`
}

type JobEvent struct {
	ID        int64  `json:"id"`
	JobID     int64  `json:"job_id"`
	TS        string `json:"ts"`
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state"`
	Detail    string `json:"detail,omitempty"`
}

type ExportChunk struct {
	JobID       int64  `json:"job_id"`
	Idx         int    `json:"idx"`
	S3Key       string `json:"s3_key"`
	RawBytes    int64  `json:"raw_bytes"`
	StoredBytes int64  `json:"stored_bytes"`
	SHA256      string `json:"sha256"`
	UploadedAt  string `json:"uploaded_at,omitempty"`
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) CreateJob(ctx context.Context, kind, label string, remoteID *int64, generation, state, trigger string) (*Job, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx, `
	    INSERT INTO job(kind, cart_label, remote_id, generation, state, trigger, created_at, updated_at)
	    VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, label, remoteID, generation, state, trigger, ts, ts)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO job_event(job_id, ts, from_state, to_state, detail) VALUES(?, ?, NULL, ?, ?)`,
		id, ts, state, "job created ("+trigger+")"); err != nil {
		return nil, err
	}
	return s.GetJob(ctx, id)
}

const jobCols = `SELECT id, kind, cart_label, remote_id, COALESCE(generation,''), state, trigger,
    bytes_total, bytes_done, chunks_total, chunks_done, COALESCE(error,''),
    created_at, updated_at, COALESCE(finished_at,''), COALESCE(system_name,''), target_library FROM job`

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	var remoteID, targetLib sql.NullInt64
	err := row.Scan(&j.ID, &j.Kind, &j.CartLabel, &remoteID, &j.Generation, &j.State, &j.Trigger,
		&j.BytesTotal, &j.BytesDone, &j.ChunksTotal, &j.ChunksDone, &j.Error,
		&j.CreatedAt, &j.UpdatedAt, &j.FinishedAt, &j.SystemName, &targetLib)
	if err != nil {
		return nil, err
	}
	if remoteID.Valid {
		j.RemoteID = &remoteID.Int64
	}
	if targetLib.Valid {
		j.TargetLibrary = &targetLib.Int64
	}
	return &j, nil
}

// SetJobImportTarget records where a foreign-cart import lands: the source
// system (disambiguates the manifest) and the destination local library.
func (s *Store) SetJobImportTarget(ctx context.Context, id int64, systemName string, targetLibrary int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job SET system_name=?, target_library=?, updated_at=? WHERE id=?`,
		systemName, targetLibrary, now(), id)
	return err
}

func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobCols+` WHERE id=?`, id))
}

func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, jobCols+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// SearchJobs queries the FULL job table (not just the recent window the
// Jobs page loads): a case-insensitive free-text match across id, kind,
// state, cart label, trigger, generation, system and error. Newest first,
// capped at limit. LIKE wildcards are escaped so a literal % matches itself.
func (s *Store) SearchJobs(ctx context.Context, query string, limit int) ([]Job, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	p := "%" + esc + "%"
	rows, err := s.db.QueryContext(ctx, jobCols+`
	    WHERE ? = ''
	       OR CAST(id AS TEXT) LIKE ? ESCAPE '\'
	       OR kind LIKE ? ESCAPE '\'
	       OR state LIKE ? ESCAPE '\'
	       OR COALESCE(cart_label,'') LIKE ? ESCAPE '\'
	       OR trigger LIKE ? ESCAPE '\'
	       OR COALESCE(generation,'') LIKE ? ESCAPE '\'
	       OR COALESCE(system_name,'') LIKE ? ESCAPE '\'
	       OR COALESCE(error,'') LIKE ? ESCAPE '\'
	    ORDER BY id DESC LIMIT ?`,
		query, p, p, p, p, p, p, p, p, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{} // never nil — the API must emit [] not null
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// UnfinishedJobs returns the export pipeline's jobs in non-terminal
// states, oldest first — the resume queue at daemon startup. Storage
// jobs (pool_create) are excluded: a half-built ZFS pool can't be
// blindly resumed; the storage manager fails them at startup instead.
func (s *Store) UnfinishedJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx,
		jobCols+` WHERE state NOT IN ('done','failed','cancelled')
		 AND kind IN ('export','import','evict') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// FailInterruptedJobs marks a kind's non-terminal jobs failed — the
// storage manager's startup sweep.
func (s *Store) FailInterruptedJobs(ctx context.Context, kind, detail string) (int64, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx, `
	    UPDATE job SET state='failed', error=?, updated_at=?, finished_at=?
	    WHERE kind=? AND state NOT IN ('done','failed','cancelled')`,
		detail, ts, ts, kind)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TransitionJob moves a job to a new state and records the transition.
// Terminal states stamp finished_at.
func (s *Store) TransitionJob(ctx context.Context, id int64, from, to, detail string) error {
	ts := now()
	fin := ""
	if to == "done" || to == "failed" || to == "cancelled" {
		fin = ts
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if fin != "" {
		_, err = tx.ExecContext(ctx, `UPDATE job SET state=?, updated_at=?, finished_at=? WHERE id=?`, to, ts, fin, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE job SET state=?, updated_at=? WHERE id=?`, to, ts, id)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_event(job_id, ts, from_state, to_state, detail) VALUES(?, ?, ?, ?, ?)`,
		id, ts, from, to, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetJobError(ctx context.Context, id int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE job SET error=?, updated_at=? WHERE id=?`, msg, now(), id)
	return err
}

func (s *Store) SetJobGeneration(ctx context.Context, id int64, generation string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE job SET generation=?, updated_at=? WHERE id=?`, generation, now(), id)
	return err
}

func (s *Store) SetJobTotals(ctx context.Context, id int64, bytesTotal int64, chunksTotal int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job SET bytes_total=?, chunks_total=?, updated_at=? WHERE id=?`,
		bytesTotal, chunksTotal, now(), id)
	return err
}

func (s *Store) SetJobProgress(ctx context.Context, id int64, bytesDone int64, chunksDone int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job SET bytes_done=?, chunks_done=?, updated_at=? WHERE id=?`,
		bytesDone, chunksDone, now(), id)
	return err
}

func (s *Store) JobEvents(ctx context.Context, jobID int64) ([]JobEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, ts, COALESCE(from_state,''), to_state, COALESCE(detail,'')
		 FROM job_event WHERE job_id=? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobEvent
	for rows.Next() {
		var e JobEvent
		if err := rows.Scan(&e.ID, &e.JobID, &e.TS, &e.FromState, &e.ToState, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordChunk marks a chunk uploaded in the resume ledger.
func (s *Store) RecordChunk(ctx context.Context, c ExportChunk) error {
	_, err := s.db.ExecContext(ctx, `
	    INSERT INTO export_chunk(job_id, idx, s3_key, raw_bytes, stored_bytes, sha256, uploaded_at)
	    VALUES(?, ?, ?, ?, ?, ?, ?)
	    ON CONFLICT(job_id, idx) DO UPDATE SET
	        s3_key=excluded.s3_key, raw_bytes=excluded.raw_bytes,
	        stored_bytes=excluded.stored_bytes, sha256=excluded.sha256,
	        uploaded_at=excluded.uploaded_at`,
		c.JobID, c.Idx, c.S3Key, c.RawBytes, c.StoredBytes, c.SHA256, now())
	return err
}

// ClearChunks drops a job's chunk ledger — used when the cart changed
// between attempts and the deterministic-tar resume assumption is void.
func (s *Store) ClearChunks(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM export_chunk WHERE job_id=?`, jobID)
	return err
}

func (s *Store) ChunksForJob(ctx context.Context, jobID int64) ([]ExportChunk, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id, idx, s3_key, raw_bytes, stored_bytes, sha256, COALESCE(uploaded_at,'')
		 FROM export_chunk WHERE job_id=? ORDER BY idx`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportChunk
	for rows.Next() {
		var c ExportChunk
		if err := rows.Scan(&c.JobID, &c.Idx, &c.S3Key, &c.RawBytes, &c.StoredBytes, &c.SHA256, &c.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCartLocalState flips a cartridge between resident and evicted and
// remembers the generation that backs an evicted stub.
func (s *Store) SetCartLocalState(ctx context.Context, label, state, lastExportGen string) error {
	if lastExportGen != "" {
		_, err := s.db.ExecContext(ctx,
			`UPDATE cartridge SET local_state=?, last_export_gen=? WHERE label=?`, state, lastExportGen, label)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE cartridge SET local_state=? WHERE label=?`, state, label)
	return err
}

type CartMeta struct {
	Label         string `json:"label"`
	LocalState    string `json:"local_state"`
	LastExportGen string `json:"last_export_gen,omitempty"`
}

func (s *Store) CartMetas(ctx context.Context) (map[string]CartMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT label, local_state, COALESCE(last_export_gen,'') FROM cartridge`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CartMeta{}
	for rows.Next() {
		var m CartMeta
		if err := rows.Scan(&m.Label, &m.LocalState, &m.LastExportGen); err != nil {
			return nil, err
		}
		out[m.Label] = m
	}
	return out, rows.Err()
}
