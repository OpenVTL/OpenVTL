package store

import (
	"context"
)

// CatalogEntry caches one manifest from a bucket. The bucket is the
// catalog of record — every row here is rebuildable from a listing, and
// RebuildCatalog-style flows may wipe + repopulate per remote.
type CatalogEntry struct {
	RemoteID      int64  `json:"remote_id"`
	SystemName    string `json:"system_name"`
	LibrarySerial string `json:"library_serial"`
	CartLabel     string `json:"cart_label"`
	Generation    string `json:"generation"`
	ManifestJSON  string `json:"-"`
	LogicalBytes  int64  `json:"logical_bytes"`
	StoredBytes   int64  `json:"stored_bytes"`
	ChunkCount    int    `json:"chunk_count"`
	ExportedAt    string `json:"exported_at"`
	SyncedAt      string `json:"synced_at"`
}

func (s *Store) UpsertCatalogEntry(ctx context.Context, e CatalogEntry) error {
	_, err := s.db.ExecContext(ctx, `
	    INSERT INTO catalog(remote_id, system_name, library_serial, cart_label, generation, manifest_json, logical_bytes, stored_bytes, chunk_count, exported_at, synced_at)
	    VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	    ON CONFLICT(remote_id, system_name, cart_label, generation) DO UPDATE SET
	        library_serial=excluded.library_serial, manifest_json=excluded.manifest_json,
	        logical_bytes=excluded.logical_bytes, stored_bytes=excluded.stored_bytes,
	        chunk_count=excluded.chunk_count, exported_at=excluded.exported_at, synced_at=excluded.synced_at`,
		e.RemoteID, e.SystemName, e.LibrarySerial, e.CartLabel, e.Generation, e.ManifestJSON,
		e.LogicalBytes, e.StoredBytes, e.ChunkCount, e.ExportedAt, now())
	return err
}

func (s *Store) ListCatalog(ctx context.Context, remoteID int64) ([]CatalogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
	    SELECT remote_id, system_name, library_serial, cart_label, generation, logical_bytes, stored_bytes, chunk_count, COALESCE(exported_at,''), synced_at
	    FROM catalog WHERE remote_id=? ORDER BY system_name, library_serial, cart_label, generation DESC`, remoteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		if err := rows.Scan(&e.RemoteID, &e.SystemName, &e.LibrarySerial, &e.CartLabel, &e.Generation,
			&e.LogicalBytes, &e.StoredBytes, &e.ChunkCount, &e.ExportedAt, &e.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetManifestJSON fetches a cached manifest. A non-empty system pins the
// lookup to that instance's subtree — required when two systems sharing a
// bucket exported the same cart label (else the match is ambiguous). An
// empty system keeps the legacy any-system match for same-system re-imports.
func (s *Store) GetManifestJSON(ctx context.Context, remoteID int64, system, label, generation string) (string, error) {
	var m string
	var err error
	if system != "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT manifest_json FROM catalog WHERE remote_id=? AND system_name=? AND cart_label=? AND generation=?`,
			remoteID, system, label, generation).Scan(&m)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT manifest_json FROM catalog WHERE remote_id=? AND cart_label=? AND generation=?`,
			remoteID, label, generation).Scan(&m)
	}
	return m, err
}

// GetCatalogEntry returns one catalog row (no manifest_json) — the import
// handler's existence/validation check before it queues a foreign pull.
func (s *Store) GetCatalogEntry(ctx context.Context, remoteID int64, system, label, generation string) (CatalogEntry, error) {
	var e CatalogEntry
	err := s.db.QueryRowContext(ctx,
		`SELECT remote_id, system_name, library_serial, cart_label, generation, logical_bytes, stored_bytes, chunk_count, COALESCE(exported_at,''), synced_at
		 FROM catalog WHERE remote_id=? AND system_name=? AND cart_label=? AND generation=?`,
		remoteID, system, label, generation).Scan(&e.RemoteID, &e.SystemName, &e.LibrarySerial, &e.CartLabel, &e.Generation,
		&e.LogicalBytes, &e.StoredBytes, &e.ChunkCount, &e.ExportedAt, &e.SyncedAt)
	return e, err
}

func (s *Store) ClearCatalog(ctx context.Context, remoteID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM catalog WHERE remote_id=?`, remoteID)
	return err
}

// AllCatalogLabels lists every cart label with exported generations on
// any remote — labels the minting autogenerator must never reuse.
func (s *Store) AllCatalogLabels(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT cart_label FROM catalog`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Audit records a mutating API action. Actor is the session username
// (v0.5); the caller's address rides along in remote_addr.
func (s *Store) Audit(ctx context.Context, actor, remoteAddr, action, subject, params string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log(ts, actor, remote_addr, action, subject, params) VALUES(?, ?, ?, ?, ?, ?)`,
		now(), actor, remoteAddr, action, subject, params)
	return err
}

type AuditEntry struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	Actor      string `json:"actor"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	Action     string `json:"action"`
	Subject    string `json:"subject,omitempty"`
	Params     string `json:"params,omitempty"`
}

func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, COALESCE(actor,''), COALESCE(remote_addr,''), action, COALESCE(subject,''), COALESCE(params,'')
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.RemoteAddr, &e.Action, &e.Subject, &e.Params); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
