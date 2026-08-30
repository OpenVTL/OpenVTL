package store

// API access keys (v0.7). Bearer-token identities
// for scripts and integrations: a key carries a role capability (same
// vocabulary as users), only its hash is stored, and the whole
// mechanism is gated behind the apikeys.enabled setting (default off).

import (
	"context"
	"time"
)

type APIKey struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	CreatedBy  string  `json:"created_by"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
}

func (s *Store) CreateAPIKey(ctx context.Context, name, role, tokenHash, createdBy string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_key(name, role, token_hash, created_by, created_at) VALUES(?, ?, ?, ?, ?)`,
		name, role, tokenHash, createdBy, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, role, created_by, created_at, last_used_at FROM api_key ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Role, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// APIKeyByHash resolves a presented token hash; ErrNotFound when the
// key does not exist (revoked keys are deleted rows).
func (s *Store) APIKeyByHash(ctx context.Context, tokenHash string) (*APIKey, error) {
	var k APIKey
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, role, created_by, created_at, last_used_at FROM api_key WHERE token_hash = ?`,
		tokenHash).Scan(&k.ID, &k.Name, &k.Role, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt)
	if err != nil {
		return nil, ErrNotFound
	}
	return &k, nil
}

// TouchAPIKey bumps last_used_at; callers throttle (once a minute is
// plenty — this is an observability field, not an audit record).
func (s *Store) TouchAPIKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_key SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) DeleteAPIKey(ctx context.Context, id int64) (string, error) {
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM api_key WHERE id = ?`, id).Scan(&name); err != nil {
		return "", ErrNotFound
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_key WHERE id = ?`, id)
	return name, err
}
