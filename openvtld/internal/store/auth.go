package store

// v0.5 auth persistence: users and sessions. Password hashing and role
// policy live in internal/auth — this layer only stores what it's given.
// Roles are plain strings here (validated in Go, not SQL) so adding a
// role never needs a table rebuild.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	CreatedAt    string `json:"created_at"`
}

var ErrNotFound = errors.New("not found")

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user(username, password_hash, role, created_at) VALUES(?, ?, ?, ?)`,
		username, passwordHash, role, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const userCols = `id, username, password_hash, role, disabled, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var dis int
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &dis, &u.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Disabled = dis != 0
	return &u, nil
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM user WHERE username = ?`, username))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM user WHERE id = ?`, id))
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM user ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// CountAdmins counts enabled admins — the last one is undeletable.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

func (s *Store) UpdateUser(ctx context.Context, id int64, role string, disabled bool) error {
	dis := 0
	if disabled {
		dis = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE user SET role = ?, disabled = ? WHERE id = ?`, role, dis, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetPassword(ctx context.Context, id int64, passwordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE user SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session WHERE user_id = ?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM user WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- sessions ---

type Session struct {
	TokenHash  string
	UserID     int64
	CreatedAt  string
	ExpiresAt  time.Time
	RemoteAddr string
}

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expires time.Time, remoteAddr string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session(token_hash, user_id, created_at, expires_at, last_seen, remote_addr)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, now(), expires.UTC().Format(time.RFC3339), now(), remoteAddr)
	return err
}

// SessionUser resolves a token hash to its live user; expired sessions
// and disabled users both come back ErrNotFound.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (*User, time.Time, error) {
	var u User
	var dis int
	var expStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.password_hash, u.role, u.disabled, u.created_at, s.expires_at
		 FROM session s JOIN user u ON u.id = s.user_id
		 WHERE s.token_hash = ?`, tokenHash).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &dis, &u.CreatedAt, &expStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("bad expires_at %q: %w", expStr, err)
	}
	if dis != 0 || time.Now().After(exp) {
		return nil, time.Time{}, ErrNotFound
	}
	u.Disabled = dis != 0
	return &u, exp, nil
}

// SlideSession pushes the expiry window forward (7-day sliding renewal;
// the middleware throttles how often this runs).
func (s *Store) SlideSession(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET expires_at = ?, last_seen = ? WHERE token_hash = ?`,
		expires.UTC().Format(time.RFC3339), now(), tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM session WHERE token_hash = ?`, tokenHash)
	return err
}

// ReapSessions drops expired rows; called opportunistically on login.
func (s *Store) ReapSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM session WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	return err
}
