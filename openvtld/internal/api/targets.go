package api

// Targets & access endpoints (v0.5; multi-port access registry since
// v0.7; FC-only since 2026-08-24, iSCSI
// removed). GET /api/targets returns one view: FC ports (all of them,
// serving or not), the port-identical LUN table, and the initiator
// REGISTRY (wwpn + alias + port scope + library scope) joined with
// live login state. Unregistered initiators are hard-denied; configfs
// ACLs unknown to the registry are surfaced as unmanaged drift, never
// deleted.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/orchestrate"
	"github.com/openvtl/openvtld/internal/store"
)

type targetsView struct {
	FC struct {
		Ports []orchestrate.PortView `json:"ports"`
		NoHBA bool                   `json:"no_hba"`
	} `json:"fc"`
	LUNs       []orchestrate.LUNView       `json:"luns"`
	Libraries  []int                       `json:"libraries"` // ids, for the scope picker
	Initiators []orchestrate.InitiatorView `json:"initiators"`
	Unmanaged  []string                    `json:"unmanaged,omitempty"`
	Error      string                      `json:"error,omitempty"`
}

func (s *Server) targetsView(r *http.Request) *targetsView {
	v := &targetsView{}
	v.FC.Ports = s.fc.PortsView(r.Context())
	v.FC.NoHBA = len(v.FC.Ports) == 0
	v.LUNs = []orchestrate.LUNView{}
	v.Libraries = []int{}
	if libs, err := inventory.ParseMhvtlConf(s.cfg.MhvtlConf); err == nil {
		for _, l := range libs {
			v.Libraries = append(v.Libraries, l.ID)
		}
		luns, err := s.fc.LUNTable(r.Context(), libs)
		if err != nil {
			v.Error = err.Error()
		}
		v.LUNs = luns
	} else {
		v.Error = err.Error()
	}
	v.Initiators = s.fc.Initiators(r.Context())
	v.Unmanaged = s.fc.UnmanagedACLs(r.Context())
	return v
}

func (s *Server) getTargets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.targetsView(r))
}

// wwpnRe: naa. + 16 hex digits (an FC WWPN in targetcli form).
var wwpnRe = regexp.MustCompile(`^naa\.[0-9a-f]{16}$`)

// normalizeInitiator validates + canonicalizes an FC WWPN.
func normalizeInitiator(in string) (string, string) {
	in = strings.ToLower(strings.TrimSpace(in))
	if !strings.HasPrefix(in, "naa.") {
		in = "naa." + strings.ReplaceAll(in, ":", "")
	}
	if !wwpnRe.MatchString(in) {
		return "", "wwpn must be 16 hex digits (naa.xxxx…, colon-separated, or bare)"
	}
	return in, ""
}

// scopesFromLists canonicalizes the request's scope lists into the
// stored comma-joined form. A list that is ABSENT from the request
// (nil / json null) means "all" (stored as the empty string); a
// PRESENT-but-empty list is an explicit "none" (store.ScopeNone) — an
// operator who unchecked every box wants no access, not wide open.
func scopesFromLists(ports []string, libraries []int) (string, string) {
	portScope := ""
	if ports != nil {
		var ps []string
		for _, p := range ports {
			if p = strings.TrimSpace(p); p != "" {
				ps = append(ps, p)
			}
		}
		if portScope = strings.Join(ps, ","); portScope == "" {
			portScope = store.ScopeNone
		}
	}
	libScope := ""
	if libraries != nil {
		var ls []string
		for _, l := range libraries {
			ls = append(ls, strconv.Itoa(l))
		}
		if libScope = strings.Join(ls, ","); libScope == "" {
			libScope = store.ScopeNone
		}
	}
	return portScope, libScope
}

// applyAccess reconciles the registry onto the live config and returns
// the refreshed view. Reconcile failure is reported in the view error,
// not a 500 — intent is recorded either way and shows as not-applied.
func (s *Server) applyAccess(w http.ResponseWriter, r *http.Request) {
	v := func() string {
		libs, err := inventory.ParseMhvtlConf(s.cfg.MhvtlConf)
		if err != nil {
			return err.Error()
		}
		if err := s.fc.ReconcileAccess(r.Context(), libs); err != nil {
			return "live apply: " + err.Error()
		}
		return ""
	}()
	out := s.targetsView(r)
	if v != "" && out.Error == "" {
		out.Error = v
	}
	writeJSON(w, 200, out)
}

// addTargetACL registers an initiator (default scopes: all ports, all
// libraries) and applies it to the live targets.
func (s *Server) addTargetACL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WWPN      string   `json:"wwpn"`
		Alias     string   `json:"alias"`
		Ports     []string `json:"ports"`
		Libraries []int    `json:"libraries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	initiator, msg := normalizeInitiator(in.WWPN)
	if msg != "" {
		badRequest(w, "%s", msg)
		return
	}
	ports, libraries := scopesFromLists(in.Ports, in.Libraries)
	if err := s.db.AddACL(r.Context(), store.InitiatorACL{
		WWPN: initiator, Alias: strings.TrimSpace(in.Alias), Fabric: store.FabricFC,
		Ports: ports, Libraries: libraries,
	}); err != nil {
		badRequest(w, "registry insert (duplicate?): %v", err)
		return
	}
	s.audit(r, "target.acl.add", initiator, map[string]any{
		"alias": in.Alias, "ports": ports, "libraries": libraries,
	})
	s.applyAccess(w, r)
}

// updateTargetACL rescopes/renames a registered initiator and applies.
// Reshaping a logged-in initiator's LUN map recreates its ACL (brief
// session bounce) — the UI warns.
func (s *Server) updateTargetACL(w http.ResponseWriter, r *http.Request) {
	initiator, msg := normalizeInitiator(r.PathValue("wwpn"))
	if msg != "" {
		badRequest(w, "%s", msg)
		return
	}
	var in struct {
		Alias     string   `json:"alias"`
		Ports     []string `json:"ports"`
		Libraries []int    `json:"libraries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	ports, libraries := scopesFromLists(in.Ports, in.Libraries)
	if err := s.db.UpdateACLScopes(r.Context(), initiator, strings.TrimSpace(in.Alias), ports, libraries); err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown initiator"})
		return
	}
	s.audit(r, "target.acl.update", initiator, map[string]any{
		"alias": in.Alias, "ports": ports, "libraries": libraries,
	})
	s.applyAccess(w, r)
}

// removeTargetACL deregisters an initiator. Refuses a logged-in one
// unless force=1 — removal drops its sessions.
func (s *Server) removeTargetACL(w http.ResponseWriter, r *http.Request) {
	initiator, msg := normalizeInitiator(r.PathValue("wwpn"))
	if msg != "" {
		badRequest(w, "%s", msg)
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if !force {
		for _, iv := range s.fc.Initiators(r.Context()) {
			if iv.WWPN == initiator && iv.LoggedIn {
				badRequest(w, "initiator %s has a live session — removing it drops the host mid-flight; pass force=1 to override", initiator)
				return
			}
		}
	}
	if err := s.db.RemoveACL(r.Context(), initiator); err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown initiator"})
		return
	}
	s.audit(r, "target.acl.remove", initiator, map[string]any{"force": force})
	s.applyAccess(w, r)
}

// putPortServing flips one FC port's serving state. Disabling drops
// that port's sessions — the UI confirms.
func (s *Server) putPortServing(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WWN     string `json:"wwn"`
		Serving bool   `json:"serving"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !wwpnRe.MatchString(in.WWN) {
		badRequest(w, "wwn (naa.<16 hex>) and serving are required")
		return
	}
	libs, err := inventory.ParseMhvtlConf(s.cfg.MhvtlConf)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := s.fc.SetPortServing(r.Context(), libs, in.WWN, in.Serving); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "target.port.serving", in.WWN, map[string]any{"serving": in.Serving})
	s.getTargets(w, r)
}

