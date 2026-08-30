package api

// v0.5 session auth. Policy is method-shaped and general (a deliberate
// forward requirement): GET = any authenticated user, mutations =
// admin. Roles are capabilities, never resource scopes. CSRF: the
// session cookie is SameSite=Lax and every mutation is a non-GET JSON
// endpoint, so cross-site form posts don't carry it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/auth"
	"github.com/openvtl/openvtld/internal/store"
)

const (
	sessionCookie = "ovtl_session"
	sessionWindow = 7 * 24 * time.Hour // sliding
	slideEvery    = 10 * time.Minute   // throttle expiry-bump writes
)

type ctxKey int

const userKey ctxKey = 0

// sessionUser returns the authenticated user on the request, or nil.
func sessionUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// public paths never require a session. /metrics stays open for
// Prometheus scrapes (gauges only, trusted network — revisit if that
// changes); the static UI must load so the login page can render.
func publicPath(r *http.Request) bool {
	p := r.URL.Path
	if p == "/healthz" || p == "/metrics" {
		return true
	}
	if p == "/api/auth/login" || p == "/api/auth/setup" || p == "/api/auth/me" {
		return true // me reports anonymous/setup state to drive the login shell
	}
	return !strings.HasPrefix(p, "/api/")
}

// requireAuth wraps the whole mux.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the session if one rides along, public path or not —
		// handlers like /api/auth/me and audit want the identity.
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			hash := auth.HashToken(c.Value)
			if u, exp, err := s.db.SessionUser(r.Context(), hash); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
				// Sliding renewal, throttled: only write when >slideEvery
				// of the window is already consumed.
				if time.Now().Add(sessionWindow).Sub(exp) > slideEvery {
					if err := s.db.SlideSession(r.Context(), hash, time.Now().Add(sessionWindow)); err != nil {
						s.log.Warn("session slide", "err", err)
					}
				}
			}
		}
		// API access keys (v0.7): Bearer ovtl_* resolves to a transient
		// role-scoped identity — only when the master toggle is on, and
		// a session cookie always wins. Audit sees actor "key:<name>".
		if sessionUser(r) == nil {
			if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ovtl_") {
				if s.db.Setting(r.Context(), settingAPIKeysEnabled, "") == "1" {
					tok := strings.TrimPrefix(ah, "Bearer ")
					if k, err := s.db.APIKeyByHash(r.Context(), auth.HashToken(tok)); err == nil {
						u := &store.User{ID: -k.ID, Username: "key:" + k.Name, Role: k.Role}
						r = r.WithContext(context.WithValue(r.Context(), userKey, u))
						s.touchAPIKey(r.Context(), k)
					} else {
						s.log.Warn("rejected unknown API key", "remote", r.RemoteAddr)
					}
				} else {
					s.log.Warn("API key presented but key auth is disabled", "remote", r.RemoteAddr)
				}
			}
		}
		if publicPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		// First-run gate: no users yet → nothing works except setup.
		if !s.setupDone.Load() {
			if n, err := s.db.CountUsers(r.Context()); err == nil && n > 0 {
				s.setupDone.Store(true)
			} else {
				writeJSON(w, 409, map[string]any{"error": "setup required", "setup_required": true})
				return
			}
		}
		u := sessionUser(r)
		if u == nil {
			writeJSON(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && u.Role != auth.RoleAdmin {
			// Self-service password change is the one non-admin mutation.
			if !(r.Method == http.MethodPut && r.URL.Path == "/api/users/"+strconv.FormatInt(u.ID, 10)+"/password") {
				writeJSON(w, 403, map[string]string{"error": "admin role required"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		Expires: expires, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// --- routes ---

type credsIn struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// authSetup creates the first admin. Only valid while the user table is
// empty; races collapse on the table's uniqueness anyway.
func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	var in credsIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if err := validateCreds(in); err != "" {
		badRequest(w, "%s", err)
		return
	}
	if n, err := s.db.CountUsers(r.Context()); err != nil {
		serverError(w, err)
		return
	} else if n > 0 {
		writeJSON(w, 403, map[string]string{"error": "setup already completed"})
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		serverError(w, err)
		return
	}
	id, err := s.db.CreateUser(r.Context(), in.Username, hash, auth.RoleAdmin)
	if err != nil {
		serverError(w, err)
		return
	}
	s.setupDone.Store(true)
	s.db.Audit(r.Context(), in.Username, r.RemoteAddr, "auth.setup", in.Username, `{"id":`+strconv.FormatInt(id, 10)+`}`)
	s.log.Info("first admin created", "user", in.Username)
	s.issueSession(w, r, id, in.Username)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var in credsIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	u, err := s.db.UserByName(r.Context(), in.Username)
	authFail := func() {
		// Same delay + response for unknown user and wrong password.
		time.Sleep(time.Second)
		s.db.Audit(r.Context(), in.Username, r.RemoteAddr, "auth.login_failed", in.Username, "")
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
	}
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			serverError(w, err)
			return
		}
		authFail()
		return
	}
	if u.Disabled || !auth.VerifyPassword(in.Password, u.PasswordHash) {
		authFail()
		return
	}
	s.db.ReapSessions(r.Context())
	s.db.Audit(r.Context(), u.Username, r.RemoteAddr, "auth.login", u.Username, "")
	s.issueSession(w, r, u.ID, u.Username)
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID int64, username string) {
	raw, hash, err := auth.NewToken()
	if err != nil {
		serverError(w, err)
		return
	}
	exp := time.Now().Add(sessionWindow)
	if err := s.db.CreateSession(r.Context(), hash, userID, exp, r.RemoteAddr); err != nil {
		serverError(w, err)
		return
	}
	setSessionCookie(w, raw, exp)
	u, err := s.db.UserByID(r.Context(), userID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"user": u})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		s.db.DeleteSession(r.Context(), auth.HashToken(c.Value))
	}
	if u := sessionUser(r); u != nil {
		s.db.Audit(r.Context(), u.Username, r.RemoteAddr, "auth.logout", u.Username, "")
	}
	setSessionCookie(w, "", time.Unix(0, 0))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// authMe drives the login shell: reports setup state, anonymous, or the
// current user. Public by design.
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	if !s.setupDone.Load() {
		if n, err := s.db.CountUsers(r.Context()); err == nil && n > 0 {
			s.setupDone.Store(true)
		} else {
			writeJSON(w, 200, map[string]any{"setup_required": true})
			return
		}
	}
	if u := sessionUser(r); u != nil {
		writeJSON(w, 200, map[string]any{"user": u})
		return
	}
	writeJSON(w, 200, map[string]any{"user": nil})
}

// --- user management (admin; role checks enforced by middleware) ---

func validateCreds(in credsIn) string {
	if len(in.Username) < 2 || len(in.Username) > 64 {
		return "username must be 2-64 characters"
	}
	for _, c := range in.Username {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "username may contain letters, digits, . _ -"
		}
	}
	if len(in.Password) < 8 {
		return "password must be at least 8 characters"
	}
	return ""
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.db.ListUsers(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if users == nil {
		users = []store.User{}
	}
	writeJSON(w, 200, users)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		credsIn
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if msg := validateCreds(in.credsIn); msg != "" {
		badRequest(w, "%s", msg)
		return
	}
	if !auth.ValidRole(in.Role) {
		badRequest(w, "role must be admin or readonly")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		serverError(w, err)
		return
	}
	id, err := s.db.CreateUser(r.Context(), in.Username, hash, in.Role)
	if err != nil {
		badRequest(w, "create user: %v", err) // likely duplicate name
		return
	}
	s.audit(r, "user.create", in.Username, map[string]any{"id": id, "role": in.Role})
	u, _ := s.db.UserByID(r.Context(), id)
	writeJSON(w, 201, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	var in struct {
		Role     string `json:"role"`
		Disabled *bool  `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	u, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown user"})
		return
	}
	role := u.Role
	if in.Role != "" {
		if !auth.ValidRole(in.Role) {
			badRequest(w, "role must be admin or readonly")
			return
		}
		role = in.Role
	}
	disabled := u.Disabled
	if in.Disabled != nil {
		disabled = *in.Disabled
	}
	// Never demote/disable the last enabled admin.
	if u.Role == auth.RoleAdmin && !u.Disabled && (role != auth.RoleAdmin || disabled) {
		if n, err := s.db.CountAdmins(r.Context()); err != nil {
			serverError(w, err)
			return
		} else if n <= 1 {
			badRequest(w, "cannot demote or disable the last admin")
			return
		}
	}
	if err := s.db.UpdateUser(r.Context(), id, role, disabled); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "user.update", u.Username, map[string]any{"id": id, "role": role, "disabled": disabled})
	out, _ := s.db.UserByID(r.Context(), id)
	writeJSON(w, 200, out)
}

// setUserPassword: admins may set anyone's; a readonly user only their
// own (the middleware carves out exactly that path).
func (s *Server) setUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	me := sessionUser(r)
	if me.Role != auth.RoleAdmin && me.ID != id {
		writeJSON(w, 403, map[string]string{"error": "may only change your own password"})
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if len(in.Password) < 8 {
		badRequest(w, "password must be at least 8 characters")
		return
	}
	u, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown user"})
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := s.db.SetPassword(r.Context(), id, hash); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "user.password", u.Username, map[string]any{"id": id})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	u, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown user"})
		return
	}
	if u.Role == auth.RoleAdmin && !u.Disabled {
		if n, err := s.db.CountAdmins(r.Context()); err != nil {
			serverError(w, err)
			return
		} else if n <= 1 {
			badRequest(w, "cannot delete the last admin")
			return
		}
	}
	if err := s.db.DeleteUser(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "user.delete", u.Username, map[string]any{"id": id})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
