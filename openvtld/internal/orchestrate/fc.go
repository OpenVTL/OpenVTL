// Package orchestrate owns the LIO target lifecycle: sg nodes renumber
// on every boot and HBA reprobe, so targets must be verified (identity,
// not labels) and rebuilt when stale. Since v0.7 it owns every FC
// port:
//
//   - every target-capable FC port serves a target with the full LUN
//     table (cabled or dark — a dark port is pre-armed), minus ports
//     the operator disables (fc.disabled_ports setting);
//   - access is an initiator REGISTRY (initiator_acl): WWPN +
//     alias + port scope + library scope; unregistered initiators are
//     hard-denied (generate_node_acls=0); library scope is realized as
//     explicit per-ACL mapped LUNs (auto_add_mapped_luns off);
//   - the CARDINAL RULE: adding port targets and reconciling ACLs is
//     ADDITIVE on the live config. clearconfig (Rebuild) is reserved
//     for a stale daemon epoch or wrong LUN identity on an EXISTING
//     target — the states where sessions are already doomed. Deploys
//     must never drop a live host session (v0.6 rule).
//
// v0.6 LUN plan (settled): LUNs allocated deterministically in
// library-id order — changer first, then drives — identical on every
// port. Verification is by LUN→device binding read from
// configfs, never by backstore name.
package orchestrate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openvtl/openvtld/internal/config"
	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/store"
	"github.com/openvtl/openvtld/internal/sysexec"
)

type FC struct {
	cfg *config.Config
	db  *store.Store
	log *slog.Logger
	// mu serializes every mutating orchestration entry point (Ensure,
	// Rebuild, ReconcileAccess, SetPortServing).
	// The boot self-heal loop, operator ACL edits and apply windows are
	// concurrent callers; interleaved targetcli runs corrupt the config
	// they're both reading (v0.8.x hardening after a live FC incident).
	mu sync.Mutex
}

func New(cfg *config.Config, db *store.Store, log *slog.Logger) *FC {
	return &FC{cfg: cfg, db: db, log: log}
}

// Result of a verification pass.
type Result struct {
	OK     bool
	Detail string
}

const configfsTarget = "/sys/kernel/config/target"

// SettingDisabledPorts: comma-separated port WWNs the operator has
// excluded from serving (the per-port toggle).
const SettingDisabledPorts = "fc.disabled_ports"

func (f *FC) tc(ctx context.Context, args ...string) error {
	_, err := sysexec.Run(ctx, 30*time.Second, "targetcli", args...)
	return err
}

// ---------------------------------------------------------------- ports ----

// HBAPresent reports whether any FC host port exists.
func (f *FC) HBAPresent() bool { return len(hostPorts()) > 0 }

func (f *FC) disabledPorts(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Split(f.db.Setting(ctx, SettingDisabledPorts, ""), ",") {
		if w = strings.TrimSpace(w); w != "" {
			out[w] = true
		}
	}
	return out
}

// ServingPorts returns the live fc_host ports that should carry a
// target (all target-capable ports minus operator-disabled ones).
func (f *FC) ServingPorts(ctx context.Context) []PortState {
	dis := f.disabledPorts(ctx)
	out := []PortState{}
	for _, p := range hostPorts() {
		if !dis[p.WWPN] {
			out = append(out, p)
		}
	}
	return out
}

// AnyFabric reports whether anything should be orchestrated.
// The product is FC-only: iSCSI is not supported by the IBM i
// initiator this product targets (see docs/why-fc-only.md).
func (f *FC) AnyFabric(ctx context.Context) bool {
	return len(f.ServingPorts(ctx)) > 0
}

// naaToColons converts targetcli's "naa.2100000e1e123456" to the
// colon-separated form the qla2xxx fabric uses in configfs
// ("21:00:00:0e:1e:12:34:56").
func naaToColons(naa string) string {
	h := strings.ToLower(strings.TrimPrefix(naa, "naa."))
	var parts []string
	for i := 0; i+2 <= len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// ------------------------------------------------------------- LUN table ---

// binding is one LUN's expected backstore: slice index = LUN number.
type binding struct {
	name string // backstore name (used at build; Verify ignores names)
	dev  string // /dev/sgN, serial-verified
	lib  int    // owning library id (library scoping)
}

// WaitDiscovery blocks until every serial declared in libs answers sg
// discovery, or timeout passes; it returns whatever is still missing.
// systemd "active" precedes SCSI registration by a beat (v0.6 lesson) —
// both the Apply restart and BOOT must settle before trusting
// discovery.
func WaitDiscovery(ctx context.Context, libs []inventory.MhvtlLibrary, timeout time.Duration) []string {
	var want []string
	for _, l := range libs {
		want = append(want, l.Serial)
		for _, d := range l.Drives {
			want = append(want, d.Serial)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		devs, _ := inventory.DiscoverSG(ctx)
		var missing []string
		for _, s := range want {
			found := false
			// SerialMatches, not equality: the 3584 changer presents a
			// zero-padded, address-suffixed serial (see inventory.SerialMatches).
			for _, d := range devs {
				if inventory.SerialMatches(d.Serial, s) {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, s)
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) {
			return missing
		}
		select {
		case <-ctx.Done():
			return missing
		case <-time.After(2 * time.Second):
		}
	}
}

// expected returns the deterministic LUN table for every *live*
// library (changer discovered by serial). A declared-but-unserved
// library (pending mhvtl restart) is skipped — it gains LUNs at the
// operator-window Rebuild after the restart. A live library with a
// missing drive is an error: building from incomplete discovery could
// present a half-library to the host.
func (f *FC) expected(ctx context.Context, libs []inventory.MhvtlLibrary) ([]binding, error) {
	// Zero declared libraries (the last one was deleted): there is
	// nothing to present, so skip discovery entirely. This is not just
	// an optimization — after mhVTL stops, the just-removed library's
	// sg nodes linger (pinned by the old pscsi backstores) and sg_vpd
	// D-states probing them; running discovery here is exactly what
	// wedged a delete-to-zero (2026-07-05). The Rebuild that calls this
	// clearconfigs to an empty target, releasing those pins.
	if len(libs) == 0 {
		return []binding{}, nil
	}
	devs, err := inventory.DiscoverSG(ctx)
	if err != nil {
		return nil, err
	}
	var out []binding
	for _, lib := range libs {
		var chgr string
		for _, d := range devs {
			if d.Type == "mediumx" && inventory.SerialMatches(d.Serial, lib.Serial) {
				chgr = d.SG
			}
		}
		if chgr == "" {
			f.log.Info("library not served yet — excluded from targets", "library", lib.ID)
			continue
		}
		out = append(out, binding{fmt.Sprintf("mhvtl_l%d_chgr", lib.ID), chgr, lib.ID})
		for i, cd := range lib.Drives {
			var sg string
			for _, d := range devs {
				if d.Type == "tape" && inventory.SerialMatches(d.Serial, cd.Serial) {
					sg = d.SG
				}
			}
			if sg == "" {
				return nil, fmt.Errorf("library %d drive %d (serial %s) not discovered — refusing to build a half-library", lib.ID, i, cd.Serial)
			}
			out = append(out, binding{fmt.Sprintf("mhvtl_l%d_drv%d", lib.ID, i), sg, lib.ID})
		}
	}
	if len(out) == 0 {
		// Libraries ARE declared but none were discovered (boot race /
		// dead daemons) — refuse rather than tear a working target down
		// to empty. The zero-declared case returned early above.
		return nil, fmt.Errorf("no live library to present")
	}
	return out, nil
}

// mappedLUNs computes an initiator's visible LUN set from its library
// scope (nil = all).
func mappedLUNs(want []binding, libScope map[int]bool) []int {
	out := []int{}
	for lun, b := range want {
		if libScope == nil || libScope[b.lib] {
			out = append(out, lun)
		}
	}
	return out
}

// lunDevices reads the live LUN→device table from a tpg dir: each
// lun/lun_N holds a symlink into a backstore whose udev_path is the
// bound device.
func lunDevices(tpg string) (map[int]string, error) {
	out := map[int]string{}
	lunDirs, err := os.ReadDir(filepath.Join(tpg, "lun"))
	if err != nil {
		return nil, err
	}
	for _, ld := range lunDirs {
		var n int
		if _, err := fmt.Sscanf(ld.Name(), "lun_%d", &n); err != nil {
			continue
		}
		links, _ := os.ReadDir(filepath.Join(tpg, "lun", ld.Name()))
		for _, li := range links {
			p := filepath.Join(tpg, "lun", ld.Name(), li.Name(), "udev_path")
			if b, err := os.ReadFile(p); err == nil {
				out[n] = strings.TrimSpace(string(b))
			}
		}
	}
	return out, nil
}

// ----------------------------------------------------------------- epoch ---

// daemonEpoch fingerprints the mhVTL daemon generation: the systemd
// start timestamps of every vtllibrary@/vtltape@ instance. pscsi
// backstores hold kernel references captured at creation; a daemon
// restart re-attaches devices that can reuse the SAME sg names while
// the captured objects are dead — no passive configfs check can see it
// (v0.6 window 1). If the epoch changed since the targets were built,
// they are stale by definition.
func daemonEpoch(ctx context.Context, libs []inventory.MhvtlLibrary) string {
	var units []string
	for _, l := range libs {
		units = append(units, fmt.Sprintf("vtllibrary@%d.service", l.ID))
		for _, d := range l.Drives {
			units = append(units, fmt.Sprintf("vtltape@%d.service", d.QueueID))
		}
	}
	if len(units) == 0 {
		return ""
	}
	out, err := sysexec.Run(ctx, 15*time.Second, "systemctl",
		append([]string{"show", "-p", "ExecMainStartTimestampMonotonic"}, units...)...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

const epochPath = "/var/lib/openvtld/fc-built-epoch"

func (f *FC) epochStale(ctx context.Context, libs []inventory.MhvtlLibrary) bool {
	epoch := daemonEpoch(ctx, libs)
	if epoch == "" {
		return false
	}
	built, err := os.ReadFile(epochPath)
	return err != nil || strings.TrimSpace(string(built)) != epoch
}

func (f *FC) stampEpoch(ctx context.Context, libs []inventory.MhvtlLibrary) {
	if epoch := daemonEpoch(ctx, libs); epoch != "" {
		if err := os.WriteFile(epochPath, []byte(epoch+"\n"), 0o644); err != nil {
			f.log.Warn("epoch stamp", "err", err)
		}
	}
}

// ------------------------------------------------------- ACL reconciling ---

// tpgRef abstracts "one place ACLs live": an FC port's tpg. tcPath is
// the targetcli ACL container; dir the configfs acls dir; aclDir maps
// an initiator id to its configfs dir name (colon-hex for FC).
type tpgRef struct {
	tcPath string
	dir    string
	aclDir func(string) string
}

func fcTPG(wwn string) tpgRef {
	return tpgRef{
		tcPath: "/qla2xxx/" + wwn + "/acls",
		dir:    filepath.Join(configfsTarget, "qla2xxx", naaToColons(wwn), "tpgt_1", "acls"),
		aclDir: naaToColons,
	}
}

// liveMappedLUNs lists the mapped_lun numbers under one ACL dir.
func liveMappedLUNs(aclPath string) []int {
	out := []int{}
	entries, _ := os.ReadDir(aclPath)
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "lun_%d", &n); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reconcileACLs makes one tpg's ACLs match intent, additively:
// scoped-in initiators exist with exactly their mapped LUNs; DB
// initiators scoped OUT of this tpg are removed; configfs ACLs unknown
// to the DB are left alone (surfaced as unmanaged drift, never
// deleted). inScope decides whether a row belongs on this tpg.
func (f *FC) reconcileACLs(ctx context.Context, ref tpgRef, rows []store.InitiatorACL,
	want []binding, inScope func(store.InitiatorACL) bool) error {
	inDB := map[string]store.InitiatorACL{}
	for _, r := range rows {
		inDB[ref.aclDir(r.WWPN)] = r
	}
	live := map[string]bool{}
	if entries, err := os.ReadDir(ref.dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				live[e.Name()] = true
			}
		}
	}
	for _, r := range rows {
		dirName := ref.aclDir(r.WWPN)
		wantHere := inScope(r)
		exists := live[dirName]
		switch {
		case wantHere && !exists:
			if err := f.tc(ctx, ref.tcPath, "create", r.WWPN, "add_mapped_luns=false"); err != nil {
				return fmt.Errorf("acl create %s: %w", r.WWPN, err)
			}
			for _, lun := range mappedLUNs(want, r.LibrarySet()) {
				if err := f.tc(ctx, ref.tcPath+"/"+r.WWPN, "create",
					fmt.Sprintf("mapped_lun=%d", lun), fmt.Sprintf("tpg_lun_or_backstore=%d", lun)); err != nil {
					return fmt.Errorf("acl %s mapped_lun %d: %w", r.WWPN, lun, err)
				}
			}
		case !wantHere && exists:
			if err := f.tc(ctx, ref.tcPath, "delete", r.WWPN); err != nil {
				return fmt.Errorf("acl delete %s: %w", r.WWPN, err)
			}
		case wantHere && exists:
			wantLUNs := mappedLUNs(want, r.LibrarySet())
			got := liveMappedLUNs(filepath.Join(ref.dir, dirName))
			if !equalInts(wantLUNs, got) {
				// Reshape via delete+create — scope changes are rare and
				// operator-driven; the UI warns when the initiator is
				// logged in.
				if err := f.tc(ctx, ref.tcPath, "delete", r.WWPN); err != nil {
					return fmt.Errorf("acl reshape delete %s: %w", r.WWPN, err)
				}
				if err := f.tc(ctx, ref.tcPath, "create", r.WWPN, "add_mapped_luns=false"); err != nil {
					return fmt.Errorf("acl reshape create %s: %w", r.WWPN, err)
				}
				for _, lun := range wantLUNs {
					if err := f.tc(ctx, ref.tcPath+"/"+r.WWPN, "create",
						fmt.Sprintf("mapped_lun=%d", lun), fmt.Sprintf("tpg_lun_or_backstore=%d", lun)); err != nil {
						return fmt.Errorf("acl %s mapped_lun %d: %w", r.WWPN, lun, err)
					}
				}
			}
		}
	}
	return nil
}

// verifyACLs is the read-only counterpart of reconcileACLs.
func verifyACLs(ref tpgRef, rows []store.InitiatorACL, want []binding,
	inScope func(store.InitiatorACL) bool) []string {
	var bad []string
	for _, r := range rows {
		p := filepath.Join(ref.dir, ref.aclDir(r.WWPN))
		_, err := os.Stat(p)
		exists := err == nil
		if inScope(r) {
			if !exists {
				bad = append(bad, "acl "+r.WWPN+" absent")
			} else if !equalInts(mappedLUNs(want, r.LibrarySet()), liveMappedLUNs(p)) {
				bad = append(bad, "acl "+r.WWPN+" mapped LUNs drifted")
			}
		} else if exists {
			bad = append(bad, "acl "+r.WWPN+" present but scoped out")
		}
	}
	return bad
}

// fcInScope: a row belongs on port wwn when its fabric is fc and its
// port scope is all (nil) or contains the port.
func fcInScope(wwn string) func(store.InitiatorACL) bool {
	return func(r store.InitiatorACL) bool {
		ps := r.PortSet()
		return ps == nil || ps[wwn]
	}
}

// ---------------------------------------------------------- FC targets -----

// createFCTarget additively builds one port's target on EXISTING
// backstores named per want (no ACLs — reconcileACLs owns those).
func (f *FC) createFCTarget(ctx context.Context, wwn string, want []binding) error {
	if err := f.tc(ctx, "/qla2xxx", "create", wwn); err != nil {
		return fmt.Errorf("target create %s: %w", wwn, err)
	}
	_ = f.tc(ctx, "/qla2xxx/"+wwn, "set", "attribute", "generate_node_acls=0", "cache_dynamic_acls=0")
	for lun, b := range want {
		if err := f.tc(ctx, "/qla2xxx/"+wwn+"/luns", "create",
			"/backstores/pscsi/"+b.name, fmt.Sprintf("lun=%d", lun)); err != nil {
			return fmt.Errorf("%s lun %d: %w", wwn, lun, err)
		}
	}
	return nil
}

// DeleteFCTarget drops one port's target (its sessions die with it) —
// the disable-port path. Backstores stay.
func (f *FC) DeleteFCTarget(ctx context.Context, wwn string) error {
	if _, err := os.Stat(filepath.Join(configfsTarget, "qla2xxx", naaToColons(wwn))); err != nil {
		return nil
	}
	if err := f.tc(ctx, "/qla2xxx", "delete", wwn); err != nil {
		return fmt.Errorf("target delete %s: %w", wwn, err)
	}
	return f.tc(ctx, "saveconfig")
}

// fcTargetBuilt reports whether a port's target exists in configfs.
func fcTargetBuilt(wwn string) bool {
	_, err := os.Stat(filepath.Join(configfsTarget, "qla2xxx", naaToColons(wwn), "tpgt_1"))
	return err == nil
}

// verifyFC checks every serving port. hard=true means only a full
// Rebuild (clearconfig) can fix it: stale epoch, or wrong LUN identity
// on an EXISTING target. Everything else is additive-repairable.
func (f *FC) verifyFC(ctx context.Context, libs []inventory.MhvtlLibrary, rows []store.InitiatorACL) (r Result, hard bool) {
	if f.epochStale(ctx, libs) {
		return Result{false, "mhVTL daemons restarted since the targets were built — pscsi bindings are stale by definition"}, true
	}
	want, err := f.expected(ctx, libs)
	if err != nil {
		return Result{false, err.Error()}, false
	}
	ports := f.ServingPorts(ctx)
	var bad []string
	for _, p := range ports {
		tpg := filepath.Join(configfsTarget, "qla2xxx", naaToColons(p.WWPN), "tpgt_1")
		if _, err := os.Stat(tpg); err != nil {
			bad = append(bad, "port "+p.WWPN+": target absent")
			continue
		}
		got, err := lunDevices(tpg)
		if err != nil {
			return Result{false, p.WWPN + " configfs read: " + err.Error()}, false
		}
		for lun, b := range want {
			if got[lun] != b.dev {
				return Result{false, fmt.Sprintf("port %s lun %d: bound=%q want=%q", p.WWPN, lun, got[lun], b.dev)}, true
			}
		}
		if len(got) != len(want) {
			return Result{false, fmt.Sprintf("port %s: %d LUNs present, %d expected", p.WWPN, len(got), len(want))}, true
		}
		bad = append(bad, verifyACLs(fcTPG(p.WWPN), rows, want, fcInScope(p.WWPN))...)
	}
	if len(bad) > 0 {
		return Result{false, strings.Join(bad, "; ")}, false
	}
	return Result{true, fmt.Sprintf("%d port(s), %d LUNs identity-verified, ACLs reconciled", len(ports), len(want))}, false
}

// liveBackstores maps udev_path -> backstore name for every live pscsi
// backstore, so additive paths reuse whatever an earlier build created.
func liveBackstores() map[string]string {
	out := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(configfsTarget, "core", "pscsi_*", "*", "udev_path"))
	for _, m := range matches {
		if b, err := os.ReadFile(m); err == nil {
			if dev := strings.TrimSpace(string(b)); dev != "" {
				out[dev] = filepath.Base(filepath.Dir(m))
			}
		}
	}
	return out
}

// ensureFCAdditive repairs everything additive: missing port targets,
// ACL drift. Never clearconfigs; live sessions on healthy ports are
// untouched.
func (f *FC) ensureFCAdditive(ctx context.Context, libs []inventory.MhvtlLibrary, rows []store.InitiatorACL) error {
	want, err := f.expected(ctx, libs)
	if err != nil {
		return err
	}
	_ = f.tc(ctx, "set", "global", "auto_add_mapped_luns=false")
	live := liveBackstores()
	for i, b := range want {
		if name, ok := live[b.dev]; ok {
			want[i].name = name
			continue
		}
		if err := f.tc(ctx, "/backstores/pscsi", "create", "name="+b.name, "dev="+b.dev); err != nil {
			return fmt.Errorf("backstore %s: %w", b.name, err)
		}
	}
	for _, p := range f.ServingPorts(ctx) {
		if !fcTargetBuilt(p.WWPN) {
			f.log.Info("adding FC target additively", "port", p.WWPN)
			if err := f.createFCTarget(ctx, p.WWPN, want); err != nil {
				return err
			}
		}
		if err := f.reconcileACLs(ctx, fcTPG(p.WWPN), rows, want, fcInScope(p.WWPN)); err != nil {
			return err
		}
	}
	if err := f.tc(ctx, "saveconfig"); err != nil {
		return fmt.Errorf("saveconfig: %w", err)
	}
	if _, err := os.Stat(epochPath); err != nil {
		f.stampEpoch(ctx, libs)
	}
	return nil
}

// --------------------------------------------------------------- rebuild ---

// Rebuild tears down and reconstructs every configured fabric with
// serial-verified bindings. clearconfig drops every session — callers
// must only reach this from boot orchestration or an operator window.
func (f *FC) Rebuild(ctx context.Context, libs []inventory.MhvtlLibrary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebuildAll(ctx, libs)
}

// rebuildAll is Rebuild without the lock (Ensure holds it already).
// Hardened after a live incident: a dead ISP wedged ONE target
// create and the whole rebuild aborted, leaving no fabric at all. Now
// the foundation (clearconfig + backstores) must succeed, but per-port
// builds are BEST-EFFORT — one dead port must not take every other
// port down. A failed port's half-created target is swept so verify
// reads "absent" (additively repairable once the port recovers)
// instead of a hard identity fault (endless clearconfig churn). The
// epoch is stamped whenever the foundation was rebuilt — those
// bindings ARE fresh even if a port failed — so follow-up Ensures
// repair additively without dropping healthy ports' sessions.
func (f *FC) rebuildAll(ctx context.Context, libs []inventory.MhvtlLibrary) error {
	ports := f.ServingPorts(ctx)
	if len(ports) == 0 {
		return fmt.Errorf("no target fabrics configured — nothing to build")
	}
	want, err := f.expected(ctx, libs)
	if err != nil {
		return err
	}
	rows, err := f.db.ListACLs(ctx, "")
	if err != nil {
		return err
	}
	fcRows := filterFabric(rows, store.FabricFC)
	f.log.Info("rebuilding target fabrics", "fc_ports", len(ports), "luns", fmt.Sprint(want))

	// clearconfig: retry transient failures (empirically exit 255 with
	// empty stderr). The known root cause — the rtslib-fb-targetctl boot
	// restore racing this rebuild — is disabled by install/deploy now;
	// the retries are cheap insurance against the next flake.
	var cerr error
	for attempt := 1; attempt <= 3; attempt++ {
		if cerr = f.tc(ctx, "clearconfig", "confirm=true"); cerr == nil {
			break
		}
		f.log.Warn("clearconfig failed — retrying", "attempt", attempt, "err", cerr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if cerr != nil {
		return fmt.Errorf("clearconfig: %w", cerr)
	}
	_ = f.tc(ctx, "set", "global", "auto_add_mapped_luns=false")
	for _, b := range want {
		if err := f.tc(ctx, "/backstores/pscsi", "create", "name="+b.name, "dev="+b.dev); err != nil {
			return fmt.Errorf("backstore %s: %w", b.name, err)
		}
	}
	var failed []string
	for _, p := range ports {
		err := f.createFCTarget(ctx, p.WWPN, want)
		if err == nil {
			err = f.reconcileACLs(ctx, fcTPG(p.WWPN), fcRows, want, fcInScope(p.WWPN))
		}
		if err != nil {
			f.log.Error("port build failed — sweeping it, continuing with remaining ports", "port", p.WWPN, "err", err)
			_ = f.tc(ctx, "/qla2xxx", "delete", p.WWPN)
			failed = append(failed, p.WWPN+": "+err.Error())
		}
	}
	if err := f.tc(ctx, "saveconfig"); err != nil {
		failed = append(failed, "saveconfig: "+err.Error())
	}
	f.stampEpoch(ctx, libs)
	if len(failed) > 0 {
		return fmt.Errorf("rebuilt with failures — %s", strings.Join(failed, "; "))
	}
	return nil
}

func filterFabric(rows []store.InitiatorACL, fabric string) []store.InitiatorACL {
	out := []store.InitiatorACL{}
	for _, r := range rows {
		if r.Fabric == fabric {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- ensure ---

// Ensure verifies every configured fabric and repairs what it can:
// hard faults (stale epoch, wrong LUN identity) mean a full Rebuild;
// everything else — missing port targets, ACL drift — is repaired
// additively so live sessions never pay for a config-only fix.
func (f *FC) Ensure(ctx context.Context, libs []inventory.MhvtlLibrary) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	ports := f.ServingPorts(ctx)
	if len(ports) == 0 {
		return Result{true, "no target fabrics configured"}
	}
	rows, err := f.db.ListACLs(ctx, "")
	if err != nil {
		return Result{false, "acl intent read: " + err.Error()}
	}
	fcRows := filterFabric(rows, store.FabricFC)

	// A failed rebuild no longer aborts the ensure: rebuildAll is
	// best-effort per port, so fall through to verify and report what
	// actually came up — a partial fabric that serves 2 of 3 ports beats
	// the old "one wedged port = no fabric at all" behaviour.
	rebuild := func(reason string) string {
		f.log.Warn("targets stale — rebuilding all fabrics", "detail", reason)
		if err := f.rebuildAll(ctx, libs); err != nil {
			f.log.Error("rebuild incomplete", "err", err)
			return err.Error()
		}
		return ""
	}

	var parts []string
	allOK := true

	r, hard := f.verifyFC(ctx, libs, fcRows)
	if !r.OK && hard {
		if msg := rebuild(r.Detail); msg != "" {
			allOK = false
			parts = append(parts, "rebuild: "+msg)
		}
		r, _ = f.verifyFC(ctx, libs, fcRows)
	} else if !r.OK {
		if err := f.ensureFCAdditive(ctx, libs, fcRows); err != nil {
			parts = append(parts, "fc ensure: "+err.Error())
		}
		r, _ = f.verifyFC(ctx, libs, fcRows)
	}
	allOK = allOK && r.OK
	parts = append(parts, "fc: "+r.Detail)

	// Latent-bomb warning, not a fault: the fabric works on flash
	// firmware until the first ISP reset (see FirmwareWarning).
	if w := FirmwareWarning(); w != "" {
		f.log.Warn(w)
		parts = append(parts, "WARNING: "+w)
	}

	res := Result{allOK, strings.Join(parts, " · ")}
	if res.OK {
		f.log.Info("target fabrics verified", "detail", res.Detail)
	}
	return res
}

// ReconcileAccess applies registry changes (add/remove/rescope) to the
// live config additively — the zero-restart ACL path for both fabrics.
func (f *FC) ReconcileAccess(ctx context.Context, libs []inventory.MhvtlLibrary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows, err := f.db.ListACLs(ctx, "")
	if err != nil {
		return err
	}
	want, err := f.expected(ctx, libs)
	if err != nil {
		return err
	}
	_ = f.tc(ctx, "set", "global", "auto_add_mapped_luns=false")
	for _, p := range f.ServingPorts(ctx) {
		if !fcTargetBuilt(p.WWPN) {
			continue // port target not up (will be ensured at boot/apply)
		}
		if err := f.reconcileACLs(ctx, fcTPG(p.WWPN), filterFabric(rows, store.FabricFC), want, fcInScope(p.WWPN)); err != nil {
			return err
		}
	}
	return f.tc(ctx, "saveconfig")
}

// SetPortServing flips one port's serving state: enable = additive
// target build + ACLs; disable = delete that port's target (its
// sessions drop — caller confirms).
func (f *FC) SetPortServing(ctx context.Context, libs []inventory.MhvtlLibrary, wwn string, serving bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	dis := f.disabledPorts(ctx)
	if serving {
		delete(dis, wwn)
	} else {
		dis[wwn] = true
	}
	var list []string
	for w := range dis {
		list = append(list, w)
	}
	sort.Strings(list)
	if err := f.db.SetSetting(ctx, SettingDisabledPorts, strings.Join(list, ",")); err != nil {
		return err
	}
	if !serving {
		return f.DeleteFCTarget(ctx, wwn)
	}
	rows, err := f.db.ListACLs(ctx, store.FabricFC)
	if err != nil {
		return err
	}
	return f.ensureFCAdditive(ctx, libs, rows)
}
