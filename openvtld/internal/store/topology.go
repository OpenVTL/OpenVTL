package store

// pool + library rows (migration 004). Libraries pair 1:1 with pools —
// the library's media_dir lives on its home pool's mountpoint, and every
// cart in the library lives on that pool. Pools are ZFS datasets since
// v0.7; the VG/LV and cache columns remain from the earlier design.

import (
	"context"
	"fmt"
)

// Pool/library state values — validated here, never a SQL CHECK.
const (
	PoolCreating = "creating"
	PoolActive   = "active"
	PoolRemoving = "removing"
	PoolError    = "error"

	LibraryPendingRestart = "pending_restart"
	LibraryActive         = "active"
)

// CacheDeviceKey holds the by-id path of THE shared cache device
// (historical, dm-cache era — unused by the ZFS storage manager).
const CacheDeviceKey = "storage.cache_device"

type Pool struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	VG               string `json:"vg"`
	DataLV           string `json:"data_lv"`
	Mountpoint       string `json:"mountpoint"`
	DataDev          string `json:"data_dev"`
	CacheSliceBytes  int64  `json:"cache_slice_bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
	State            string `json:"state"`
	Detail           string `json:"detail,omitempty"`
	CreatedAt        string `json:"created_at"`
}

type Library struct {
	ID          int    `json:"id"` // the mhVTL library id (10, 20, …)
	Name        string `json:"name"`
	Vendor      string `json:"vendor"`
	Product     string `json:"product"`
	Variant     string `json:"variant,omitempty"`
	Serial      string `json:"serial"`
	DriveModel  string `json:"drive_model"`
	NumDrives   int    `json:"num_drives"`
	LabelPrefix string `json:"label_prefix"`
	MediaDir    string `json:"media_dir"`
	HomePool    int64  `json:"home_pool"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
}

const poolCols = `id, name, vg, data_lv, mountpoint, data_dev,
	cache_slice_bytes, virtual_size_bytes, state, COALESCE(detail,''), created_at`

func scanPool(sc interface{ Scan(...any) error }) (Pool, error) {
	var p Pool
	err := sc.Scan(&p.ID, &p.Name, &p.VG, &p.DataLV, &p.Mountpoint, &p.DataDev,
		&p.CacheSliceBytes, &p.VirtualSizeBytes, &p.State, &p.Detail, &p.CreatedAt)
	return p, err
}

func (s *Store) ListPools(ctx context.Context) ([]Pool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+poolCols+` FROM pool ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPool(ctx context.Context, id int64) (Pool, error) {
	p, err := scanPool(s.db.QueryRowContext(ctx,
		`SELECT `+poolCols+` FROM pool WHERE id = ?`, id))
	if err != nil {
		return Pool{}, ErrNotFound
	}
	return p, nil
}

// CreatePool registers a pool row (normally state=creating; the
// pool_create job flips it to active/error as the stack comes up).
func (s *Store) CreatePool(ctx context.Context, p Pool) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
	    INSERT INTO pool(name, vg, data_lv, mountpoint, data_dev,
	        cache_slice_bytes, virtual_size_bytes, state, detail, created_at)
	    VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.VG, p.DataLV, p.Mountpoint, p.DataDev,
		p.CacheSliceBytes, p.VirtualSizeBytes, p.State, p.Detail, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetPoolState(ctx context.Context, id int64, state, detail string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pool SET state = ?, detail = ? WHERE id = ?`, state, detail, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePool removes a pool row; refused while a library still lives on
// it (the storage teardown itself is the caller's problem).
func (s *Store) DeletePool(ctx context.Context, id int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM library WHERE home_pool = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("pool %d still home to %d library(ies)", id, n)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM pool WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return ErrNotFound
	}
	return nil
}

const libCols = `id, name, vendor, product, COALESCE(variant,''), serial,
	drive_model, num_drives, label_prefix, media_dir, home_pool, state, created_at`

func scanLibrary(sc interface{ Scan(...any) error }) (Library, error) {
	var l Library
	err := sc.Scan(&l.ID, &l.Name, &l.Vendor, &l.Product, &l.Variant, &l.Serial,
		&l.DriveModel, &l.NumDrives, &l.LabelPrefix, &l.MediaDir, &l.HomePool,
		&l.State, &l.CreatedAt)
	return l, err
}

func (s *Store) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+libCols+` FROM library ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLibrary(ctx context.Context, id int) (Library, error) {
	l, err := scanLibrary(s.db.QueryRowContext(ctx,
		`SELECT `+libCols+` FROM library WHERE id = ?`, id))
	if err != nil {
		return Library{}, ErrNotFound
	}
	return l, nil
}

func (s *Store) CreateLibrary(ctx context.Context, l Library) error {
	_, err := s.db.ExecContext(ctx, `
	    INSERT INTO library(id, name, vendor, product, variant, serial,
	        drive_model, num_drives, label_prefix, media_dir, home_pool,
	        state, created_at)
	    VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Name, l.Vendor, l.Product, l.Variant, l.Serial,
		l.DriveModel, l.NumDrives, l.LabelPrefix, l.MediaDir, l.HomePool,
		l.State, now())
	return err
}

func (s *Store) SetLibraryState(ctx context.Context, id int, state string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE library SET state = ? WHERE id = ?`, state, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CartLabelsByLibrary lists persisted cart labels for a library —
// cascade deletion's fallback source when the live snapshot has
// already forgotten the library (retry after a partial failure).
func (s *Store) CartLabelsByLibrary(ctx context.Context, libID int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT label FROM cartridge WHERE library_id = ?`, libID)
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

func (s *Store) DeleteLibrary(ctx context.Context, id int) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM library WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
