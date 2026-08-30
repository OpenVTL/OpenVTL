package api

// Support licensing key (v0.7 post-release): a fingerprint the customer
// gives support to link this appliance to a paid account. It is DERIVED
// from the machine's durable identity (internal/license), not stored as
// authoritative and not user-editable. We persist only the last-seen value
// as a change detector: when a re-key happens (OS reinstall or hardware
// transfer), the UI prompts the user to update it on their support profile.
// Nothing is ever uploaded from here.

import (
	"net/http"

	"github.com/openvtl/openvtld/internal/license"
)

const settingLicenseSeen = "license.seen"

func (s *Server) licenseInfo(w http.ResponseWriter, r *http.Request) {
	cur, err := license.Compute()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "cannot read machine identity: " + err.Error()})
		return
	}
	seen := s.db.Setting(r.Context(), settingLicenseSeen, "")
	changed, previous := false, ""
	switch {
	case seen == "":
		// First sight: establish the baseline silently so a later change is
		// detectable even though the user attaches the key on the portal,
		// not in-app.
		if err := s.db.SetSetting(r.Context(), settingLicenseSeen, cur); err != nil {
			s.log.Warn("license baseline", "err", err)
		}
	case seen != cur:
		changed, previous = true, seen
	}
	writeJSON(w, 200, map[string]any{"fingerprint": cur, "changed": changed, "previous": previous})
}

// licenseAck records the current key as acknowledged — the user has updated
// it on their support profile, clearing the change notice. Admin (POST is
// gated by requireAuth).
func (s *Server) licenseAck(w http.ResponseWriter, r *http.Request) {
	cur, err := license.Compute()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := s.db.SetSetting(r.Context(), settingLicenseSeen, cur); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "license.ack", cur, nil)
	writeJSON(w, 200, map[string]any{"ok": true, "fingerprint": cur})
}
