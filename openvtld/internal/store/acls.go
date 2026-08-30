package store

// initiator_acl is the ACCESS REGISTRY (v0.7): what the operator
// wants presented, to whom. configfs is
// reality; orchestrate reconciles the two. Each row: an initiator
// (an FC WWPN, naa. form) + alias + two scopes, both '' = all:
//   ports     — comma-sep target port WWNs the initiator is admitted on
//   libraries — comma-sep library ids whose LUNs it sees (mapped LUNs)
// Unregistered initiators are hard-denied (generate_node_acls=0).
// The fabric column predates the FC-only product (iSCSI removed
// 2026-08-24); rows with any other fabric value are ignored upstream.

import (
	"context"
	"strconv"
	"strings"
)

const FabricFC = "fc"

// ScopeNone is the stored sentinel for an explicitly EMPTY scope (the
// operator unchecked every box) — distinct from the empty string,
// which means "all". A no-ports initiator is removed from every tpg
// (denied everywhere); a no-libraries one may log in but sees no LUNs.
const ScopeNone = "-"

type InitiatorACL struct {
	WWPN      string `json:"wwpn"` // naa. form (targetcli)
	Alias     string `json:"alias"`
	Fabric    string `json:"fabric"`
	Ports     string `json:"ports"`     // '' = all target ports; '-' = none
	Libraries string `json:"libraries"` // '' = all libraries; '-' = none
	CreatedAt string `json:"created_at"`
}

// PortSet returns the port scope as a set; nil means "all ports", an
// empty non-nil set means "no ports" (ScopeNone).
func (a InitiatorACL) PortSet() map[string]bool {
	s := strings.TrimSpace(a.Ports)
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	if s == ScopeNone {
		return out
	}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// LibrarySet returns the library scope as a set; nil means "all", an
// empty non-nil set means "no libraries" (ScopeNone).
func (a InitiatorACL) LibrarySet() map[int]bool {
	s := strings.TrimSpace(a.Libraries)
	if s == "" {
		return nil
	}
	out := map[int]bool{}
	if s == ScopeNone {
		return out
	}
	for _, l := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(l)); err == nil {
			out[n] = true
		}
	}
	return out
}

// ListACLs returns rows for one fabric, or all when fabric is "".
func (s *Store) ListACLs(ctx context.Context, fabric string) ([]InitiatorACL, error) {
	q := `SELECT wwpn, alias, fabric, ports, libraries, created_at FROM initiator_acl`
	var args []any
	if fabric != "" {
		q += ` WHERE fabric = ?`
		args = append(args, fabric)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY wwpn`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InitiatorACL
	for rows.Next() {
		var a InitiatorACL
		if err := rows.Scan(&a.WWPN, &a.Alias, &a.Fabric, &a.Ports, &a.Libraries, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddACL(ctx context.Context, a InitiatorACL) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO initiator_acl(wwpn, alias, fabric, ports, libraries, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		a.WWPN, a.Alias, a.Fabric, a.Ports, a.Libraries, now())
	return err
}

// UpdateACLScopes rewrites alias + scopes for one initiator.
func (s *Store) UpdateACLScopes(ctx context.Context, wwpn, alias, ports, libraries string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE initiator_acl SET alias = ?, ports = ?, libraries = ? WHERE wwpn = ?`,
		alias, ports, libraries, wwpn)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RemoveACL(ctx context.Context, wwpn string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM initiator_acl WHERE wwpn = ?`, wwpn)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SeedACLs bootstraps the table from the -acls flag exactly once (a
// settings marker gates it): a WWPN the operator later removes must
// stay removed across restarts, so this can't be a plain upsert.
// Flag-seeded entries are FC, all ports, all libraries.
func (s *Store) SeedACLs(ctx context.Context, wwpns []string) error {
	const marker = "acl.seeded"
	if s.Setting(ctx, marker, "") == "1" {
		return nil
	}
	for _, w := range wwpns {
		if w = strings.TrimSpace(w); w == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO initiator_acl(wwpn, alias, fabric, ports, libraries, created_at) VALUES(?, '', 'fc', '', '', ?)`,
			w, now()); err != nil {
			return err
		}
	}
	return s.SetSetting(ctx, marker, "1")
}
