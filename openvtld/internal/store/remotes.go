package store

import (
	"context"
	"database/sql"
	"time"
)

// Remote is an S3 endpoint configuration. SecretKey never leaves the
// daemon: the API layer must redact it before serializing a Remote.
type Remote struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	AccessKey      string `json:"access_key"`
	SecretKey      string `json:"-"`
	UseSSL         bool   `json:"use_ssl"`
	PathStyle      bool   `json:"path_style"`
	CreatedAt      string `json:"created_at"`
	LastTestAt     string `json:"last_test_at,omitempty"`
	LastTestOK     *bool  `json:"last_test_ok,omitempty"`
	LastTestDetail string `json:"last_test_detail,omitempty"`
}

func (s *Store) CreateRemote(ctx context.Context, r *Remote) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
	    INSERT INTO s3_remote(name, endpoint, region, bucket, prefix, access_key, secret_key, use_ssl, path_style, created_at)
	    VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Endpoint, r.Region, r.Bucket, r.Prefix, r.AccessKey, r.SecretKey,
		boolInt(r.UseSSL), boolInt(r.PathStyle), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateRemote(ctx context.Context, r *Remote) error {
	// Empty SecretKey means "keep the stored one" so edits from the UI
	// never require re-entering the secret.
	if r.SecretKey == "" {
		_, err := s.db.ExecContext(ctx, `
		    UPDATE s3_remote SET name=?, endpoint=?, region=?, bucket=?, prefix=?, access_key=?, use_ssl=?, path_style=?
		    WHERE id=?`,
			r.Name, r.Endpoint, r.Region, r.Bucket, r.Prefix, r.AccessKey,
			boolInt(r.UseSSL), boolInt(r.PathStyle), r.ID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	    UPDATE s3_remote SET name=?, endpoint=?, region=?, bucket=?, prefix=?, access_key=?, secret_key=?, use_ssl=?, path_style=?
	    WHERE id=?`,
		r.Name, r.Endpoint, r.Region, r.Bucket, r.Prefix, r.AccessKey, r.SecretKey,
		boolInt(r.UseSSL), boolInt(r.PathStyle), r.ID)
	return err
}

func (s *Store) DeleteRemote(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM s3_remote WHERE id=?`, id)
	return err
}

func (s *Store) GetRemote(ctx context.Context, id int64) (*Remote, error) {
	return s.scanRemote(s.db.QueryRowContext(ctx, remoteCols+` WHERE id=?`, id))
}

func (s *Store) ListRemotes(ctx context.Context) ([]Remote, error) {
	rows, err := s.db.QueryContext(ctx, remoteCols+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Remote
	for rows.Next() {
		r, err := s.scanRemote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) RecordRemoteTest(ctx context.Context, id int64, ok bool, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE s3_remote SET last_test_at=?, last_test_ok=?, last_test_detail=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), boolInt(ok), detail, id)
	return err
}

const remoteCols = `SELECT id, name, endpoint, region, bucket, prefix, access_key, secret_key,
    use_ssl, path_style, created_at, COALESCE(last_test_at,''), last_test_ok, COALESCE(last_test_detail,'')
    FROM s3_remote`

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanRemote(row rowScanner) (*Remote, error) {
	var r Remote
	var useSSL, pathStyle int
	var testOK sql.NullInt64
	err := row.Scan(&r.ID, &r.Name, &r.Endpoint, &r.Region, &r.Bucket, &r.Prefix,
		&r.AccessKey, &r.SecretKey, &useSSL, &pathStyle, &r.CreatedAt,
		&r.LastTestAt, &testOK, &r.LastTestDetail)
	if err != nil {
		return nil, err
	}
	r.UseSSL, r.PathStyle = useSSL != 0, pathStyle != 0
	if testOK.Valid {
		b := testOK.Int64 != 0
		r.LastTestOK = &b
	}
	return &r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
