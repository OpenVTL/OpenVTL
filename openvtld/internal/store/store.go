// Package store is the SQLite persistence layer (modernc.org/sqlite,
// pure Go — keeps the binary CGO-free and cross-compilable).
//
// v0.3 persists what must survive a restart: cartridge history and the
// event log. Live library topology is observable and kept in memory by
// the inventory engine. Jobs/auth tables land in v0.4/v0.5.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

var migrations = []string{
	// 001 — v0.3 baseline (idempotent by construction: pre-gating DBs
	// re-run it once before schema_version starts gating).
	`CREATE TABLE IF NOT EXISTS cartridge (
	    label       TEXT PRIMARY KEY,
	    size_bytes  INTEGER NOT NULL DEFAULT 0,
	    last_write  TEXT,
	    last_seen   TEXT,
	    status      TEXT NOT NULL DEFAULT 'slot'
	);
	CREATE TABLE IF NOT EXISTS event_log (
	    id      INTEGER PRIMARY KEY AUTOINCREMENT,
	    ts      TEXT NOT NULL,
	    kind    TEXT NOT NULL,
	    subject TEXT NOT NULL,
	    detail  TEXT
	);
	CREATE INDEX IF NOT EXISTS event_log_ts ON event_log(ts);
	CREATE TABLE IF NOT EXISTS settings (
	    key   TEXT PRIMARY KEY,
	    value TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL);`,

	// 002 — v0.4 export/import pipeline
	`CREATE TABLE s3_remote (
	    id           INTEGER PRIMARY KEY AUTOINCREMENT,
	    name         TEXT NOT NULL UNIQUE,
	    endpoint     TEXT NOT NULL DEFAULT 's3.amazonaws.com',
	    region       TEXT NOT NULL,
	    bucket       TEXT NOT NULL,
	    prefix       TEXT NOT NULL DEFAULT '',
	    access_key   TEXT NOT NULL,
	    secret_key   TEXT NOT NULL,
	    use_ssl      INTEGER NOT NULL DEFAULT 1,
	    path_style   INTEGER NOT NULL DEFAULT 0,
	    created_at   TEXT NOT NULL,
	    last_test_at TEXT,
	    last_test_ok INTEGER,
	    last_test_detail TEXT
	);
	CREATE TABLE job (
	    id           INTEGER PRIMARY KEY AUTOINCREMENT,
	    kind         TEXT NOT NULL CHECK(kind IN ('export','import','evict')),
	    cart_label   TEXT NOT NULL,
	    remote_id    INTEGER REFERENCES s3_remote(id),
	    generation   TEXT,
	    state        TEXT NOT NULL,
	    trigger      TEXT NOT NULL,
	    bytes_total  INTEGER NOT NULL DEFAULT 0,
	    bytes_done   INTEGER NOT NULL DEFAULT 0,
	    chunks_total INTEGER NOT NULL DEFAULT 0,
	    chunks_done  INTEGER NOT NULL DEFAULT 0,
	    error        TEXT,
	    created_at   TEXT NOT NULL,
	    updated_at   TEXT NOT NULL,
	    finished_at  TEXT
	);
	CREATE INDEX job_state ON job(state);
	CREATE TABLE job_event (
	    id        INTEGER PRIMARY KEY AUTOINCREMENT,
	    job_id    INTEGER NOT NULL REFERENCES job(id),
	    ts        TEXT NOT NULL,
	    from_state TEXT,
	    to_state  TEXT NOT NULL,
	    detail    TEXT
	);
	CREATE INDEX job_event_job ON job_event(job_id);
	CREATE TABLE export_chunk (
	    job_id       INTEGER NOT NULL REFERENCES job(id),
	    idx          INTEGER NOT NULL,
	    s3_key       TEXT NOT NULL,
	    raw_bytes    INTEGER NOT NULL,
	    stored_bytes INTEGER NOT NULL,
	    sha256       TEXT NOT NULL,
	    uploaded_at  TEXT,
	    PRIMARY KEY (job_id, idx)
	);
	CREATE TABLE catalog (
	    remote_id     INTEGER NOT NULL REFERENCES s3_remote(id),
	    cart_label    TEXT NOT NULL,
	    generation    TEXT NOT NULL,
	    manifest_json TEXT NOT NULL,
	    logical_bytes INTEGER,
	    stored_bytes  INTEGER,
	    chunk_count   INTEGER,
	    exported_at   TEXT,
	    synced_at     TEXT NOT NULL,
	    PRIMARY KEY (remote_id, cart_label, generation)
	);
	CREATE TABLE audit_log (
	    id      INTEGER PRIMARY KEY AUTOINCREMENT,
	    ts      TEXT NOT NULL,
	    actor   TEXT,
	    action  TEXT NOT NULL,
	    subject TEXT,
	    params  TEXT
	);
	ALTER TABLE cartridge ADD COLUMN local_state TEXT NOT NULL DEFAULT 'resident';
	ALTER TABLE cartridge ADD COLUMN last_export_gen TEXT;`,

	// 003 — v0.5 auth, ACL intent, persisted pool samples.
	// Role values are validated in Go, not a CHECK constraint: adding a
	// role later must not need a table rebuild (deliberate forward
	// requirement). Sessions store only the token hash — the raw token
	// exists in the client cookie and nowhere else.
	`CREATE TABLE user (
	    id            INTEGER PRIMARY KEY AUTOINCREMENT,
	    username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
	    password_hash TEXT NOT NULL,
	    role          TEXT NOT NULL,
	    disabled      INTEGER NOT NULL DEFAULT 0,
	    created_at    TEXT NOT NULL
	);
	CREATE TABLE session (
	    token_hash  TEXT PRIMARY KEY,
	    user_id     INTEGER NOT NULL REFERENCES user(id),
	    created_at  TEXT NOT NULL,
	    expires_at  TEXT NOT NULL,
	    last_seen   TEXT,
	    remote_addr TEXT
	);
	CREATE INDEX session_expiry ON session(expires_at);
	CREATE TABLE initiator_acl (
	    wwpn       TEXT PRIMARY KEY,
	    alias      TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL
	);
	CREATE TABLE dedupe_stats (
	    ts             TEXT NOT NULL,
	    pool           TEXT NOT NULL DEFAULT '',
	    fs_used_bytes  INTEGER,
	    fs_total_bytes INTEGER,
	    vdo_used_bytes INTEGER,
	    vdo_phys_bytes INTEGER,
	    vdo_saving_pct INTEGER,
	    cache_used_pct REAL,
	    PRIMARY KEY (pool, ts)
	);
	ALTER TABLE audit_log ADD COLUMN remote_addr TEXT;`,

	// 004 — v0.6 multi-library & storage pools. (Historical: the
	// dm-cache design this migration served is gone; pools are ZFS
	// datasets since v0.7.) A pool is one deduped
	// filesystem (data disk + optional slice of THE shared cache device,
	// which lives in the settings table as storage.cache_device — one per
	// system, never a pool itself). Libraries pair 1:1 with pools; a
	// cart's pool is its library's pool, so cartridge carries only
	// library_id. label stays the PK — labels are globally unique
	// (settled: matches barcode reality, keeps jobs/catalog/S3
	// label-addressed). State values validated in Go, no CHECK.
	`CREATE TABLE pool (
	    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	    name               TEXT NOT NULL UNIQUE,
	    vg                 TEXT NOT NULL,
	    data_lv            TEXT NOT NULL,
	    mountpoint         TEXT NOT NULL UNIQUE,
	    data_dev           TEXT NOT NULL,
	    cache_slice_bytes  INTEGER NOT NULL DEFAULT 0,
	    virtual_size_bytes INTEGER NOT NULL DEFAULT 0,
	    state              TEXT NOT NULL,
	    detail             TEXT NOT NULL DEFAULT '',
	    created_at         TEXT NOT NULL
	);
	CREATE TABLE library (
	    id           INTEGER PRIMARY KEY,
	    name         TEXT NOT NULL UNIQUE,
	    vendor       TEXT NOT NULL,
	    product      TEXT NOT NULL,
	    variant      TEXT NOT NULL DEFAULT '',
	    serial       TEXT NOT NULL UNIQUE,
	    drive_model  TEXT NOT NULL,
	    num_drives   INTEGER NOT NULL,
	    label_prefix TEXT NOT NULL,
	    media_dir    TEXT NOT NULL,
	    home_pool    INTEGER NOT NULL REFERENCES pool(id),
	    state        TEXT NOT NULL,
	    created_at   TEXT NOT NULL
	);
	ALTER TABLE cartridge ADD COLUMN library_id INTEGER;`,

	// 005 — v0.6 pool builder: job.kind gains 'pool_create', which the
	// 002 CHECK constraint forbids. Rebuild the table without it — kind
	// values are validated in Go from here on (same no-table-rebuild
	// rule as roles). job_event/export_chunk FKs aren't enforced (foreign_keys
	// pragma is off), so drop+rename preserves ids safely.
	`CREATE TABLE job_new (
	    id           INTEGER PRIMARY KEY AUTOINCREMENT,
	    kind         TEXT NOT NULL,
	    cart_label   TEXT NOT NULL,
	    remote_id    INTEGER REFERENCES s3_remote(id),
	    generation   TEXT,
	    state        TEXT NOT NULL,
	    trigger      TEXT NOT NULL,
	    bytes_total  INTEGER NOT NULL DEFAULT 0,
	    bytes_done   INTEGER NOT NULL DEFAULT 0,
	    chunks_total INTEGER NOT NULL DEFAULT 0,
	    chunks_done  INTEGER NOT NULL DEFAULT 0,
	    error        TEXT,
	    created_at   TEXT NOT NULL,
	    updated_at   TEXT NOT NULL,
	    finished_at  TEXT
	);
	INSERT INTO job_new SELECT * FROM job;
	DROP TABLE job;
	ALTER TABLE job_new RENAME TO job;
	CREATE INDEX job_state ON job(state);`,

	// 006 — v0.7 iSCSI fabric: initiator_acl rows carry which fabric
	// they belong to (fc = WWPN, iscsi = IQN). Existing rows are FC.
	`ALTER TABLE initiator_acl ADD COLUMN fabric TEXT NOT NULL DEFAULT 'fc';`,

	// 007 — v0.7 access scoping: each
	// initiator carries a port scope (comma-sep target WWNs, '' = all
	// ports) and a library scope (comma-sep library ids, '' = all —
	// realized as per-ACL mapped LUNs). Existing rows default to all.
	`ALTER TABLE initiator_acl ADD COLUMN ports TEXT NOT NULL DEFAULT '';
	ALTER TABLE initiator_acl ADD COLUMN libraries TEXT NOT NULL DEFAULT '';`,

	// 008 — v0.7 API access keys: named bearer
	// tokens with a role capability, gated by the apikeys.enabled
	// setting (default off). Only the SHA-256 hash is stored — the raw
	// token is shown once at creation. Role validated in Go, no CHECK.
	`CREATE TABLE api_key (
	    id           INTEGER PRIMARY KEY AUTOINCREMENT,
	    name         TEXT NOT NULL UNIQUE COLLATE NOCASE,
	    role         TEXT NOT NULL,
	    token_hash   TEXT NOT NULL UNIQUE,
	    created_by   TEXT NOT NULL DEFAULT '',
	    created_at   TEXT NOT NULL,
	    last_used_at TEXT
	);`,

	// 009 — v0.7 S3 namespacing (System > Library > Tape). The catalog is
	// keyed by system + library so instances sharing a bucket don't
	// collide on cart label. It is a rebuildable cache and the layout
	// ships with a clean-bucket cutover, so the table is recreated rather
	// than migrated in place.
	`DROP TABLE IF EXISTS catalog;
	CREATE TABLE catalog (
	    remote_id      INTEGER NOT NULL REFERENCES s3_remote(id),
	    system_name    TEXT NOT NULL DEFAULT '',
	    library_serial TEXT NOT NULL DEFAULT '',
	    cart_label     TEXT NOT NULL,
	    generation     TEXT NOT NULL,
	    manifest_json  TEXT NOT NULL,
	    logical_bytes  INTEGER,
	    stored_bytes   INTEGER,
	    chunk_count    INTEGER,
	    exported_at    TEXT,
	    synced_at      TEXT NOT NULL,
	    PRIMARY KEY (remote_id, system_name, cart_label, generation)
	);`,

	// 010 — v0.7 capacity trend: persist per-pool logical (pre-dedup/
	// compress) bytes. Under ZFS both statfs and zfs `used` are physical
	// (compressed), so the old fs_used series was physical, not logical —
	// the trend needs the real logical to show the saving gap.
	`ALTER TABLE dedupe_stats ADD COLUMN logical_bytes INTEGER NOT NULL DEFAULT 0;`,

	// 011 — v0.7 cross-instance import (Phase A): an import may pull a
	// FOREIGN cart (another system's export in a shared bucket) into a
	// chosen local library. system_name disambiguates the manifest when
	// two systems share a cart label; target_library is the destination
	// the cart is slotted into. Both NULL for a same-system re-import.
	`ALTER TABLE job ADD COLUMN system_name TEXT;
	ALTER TABLE job ADD COLUMN target_library INTEGER;`,
}

func Open(path string) (*Store, error) {
	// WAL + busy timeout: the daemon is the only writer, but SSE/REST
	// readers overlap collector writes constantly.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc/sqlite: single writer connection avoids SQLITE_BUSY churn
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// migrate applies pending migrations gated on schema_version. v0.3 DBs
// predate gating and have an empty schema_version; migration 001 is
// idempotent, so treating them as version 0 re-runs it harmlessly once.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("schema_version: %w", err)
	}
	var applied int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&applied); err != nil {
		return fmt.Errorf("schema_version read: %w", err)
	}
	for i := applied; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(v) VALUES(?)`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d version stamp: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d commit: %w", i+1, err)
		}
	}
	return nil
}

// Setting reads a settings-table value; missing keys return the default.
func (s *Store) Setting(ctx context.Context, key, def string) string {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AllSettings returns every settings row as a map — used by the support
// bundle. Callers redact any secret-bearing keys before emitting.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// System identity setting keys (S3 namespacing, v0.7).
const (
	SettingSystemName = "system.name"          // friendly instance name (S3 path segment)
	SettingSystemUUID = "system.instance_uuid" // stable, auto-generated backstop
)

// SystemIdentity returns the friendly system name and the stable instance
// UUID, generating + persisting the UUID on first call. nameDefault (a
// sanitized hostname) is returned when the operator hasn't set a name.
func (s *Store) SystemIdentity(ctx context.Context, nameDefault string) (name, uuid string, err error) {
	uuid = s.Setting(ctx, SettingSystemUUID, "")
	if uuid == "" {
		uuid = newUUID()
		if err = s.SetSetting(ctx, SettingSystemUUID, uuid); err != nil {
			return "", "", err
		}
	}
	name = s.Setting(ctx, SettingSystemName, "")
	if name == "" {
		name = nameDefault
	}
	return name, uuid, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpsertCartridge(ctx context.Context, label string, sizeBytes int64, status string, lastWrite *time.Time, libraryID int) error {
	var lw any
	if lastWrite != nil {
		lw = lastWrite.UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `
	    INSERT INTO cartridge(label, size_bytes, last_write, last_seen, status, library_id)
	    VALUES(?, ?, ?, ?, ?, ?)
	    ON CONFLICT(label) DO UPDATE SET
	        size_bytes = excluded.size_bytes,
	        last_write = COALESCE(excluded.last_write, cartridge.last_write),
	        last_seen  = excluded.last_seen,
	        status     = excluded.status,
	        library_id = excluded.library_id`,
		label, sizeBytes, lw, time.Now().UTC().Format(time.RFC3339), status, libraryID)
	return err
}

// DeleteCartridge removes the persisted row (meta included) after a
// live cart deletion; the media scan would otherwise resurrect stale
// local_state if the label is ever re-minted.
func (s *Store) DeleteCartridge(ctx context.Context, label string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cartridge WHERE label = ?`, label)
	return err
}

func (s *Store) LogEvent(ctx context.Context, ts time.Time, kind, subject, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO event_log(ts, kind, subject, detail) VALUES(?, ?, ?, ?)`,
		ts.UTC().Format(time.RFC3339Nano), kind, subject, detail)
	return err
}

type LoggedEvent struct {
	ID      int64  `json:"id"`
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail,omitempty"`
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]LoggedEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, kind, subject, COALESCE(detail,'') FROM event_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoggedEvent{} // never nil — the API must emit [] not null
	for rows.Next() {
		var e LoggedEvent
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SearchEvents queries the FULL persisted event_log, not just the recent
// window — an optional exact kind filter plus a case-insensitive free-text
// match on subject/detail. Powers the journal viewer's "search all history"
// (the recent-events feed only holds the newest rows). Results are newest
// first, capped at limit.
func (s *Store) SearchEvents(ctx context.Context, query, kind string, limit int) ([]LoggedEvent, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	// Escape LIKE wildcards so a literal % or _ typed in the query matches
	// itself rather than acting as a wildcard.
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	pattern := "%" + esc + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, kind, subject, COALESCE(detail,'')
		   FROM event_log
		  WHERE (? = '' OR kind = ?)
		    AND (? = '' OR subject LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\')
		  ORDER BY id DESC LIMIT ?`,
		kind, kind, query, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoggedEvent{} // never nil — a zero-match search must emit [] not null
	for rows.Next() {
		var e LoggedEvent
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
