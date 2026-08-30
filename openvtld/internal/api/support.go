package api

// Support log bundle (v0.7 post-release, feature #3): a redacted .tar.gz of
// diagnostics an operator hands to support. It carries logs, status and
// storage state, plus an operational-identity block (feature #4's clone
// detection) — history that can't be collided by pinning SMBIOS/machine-id,
// so support can spot one fingerprint running on many instances.
//
// REDACTION IS THE CONTRACT: the bundle never contains secrets. We read only
// curated, non-secret extracts — never the SQLite file, S3 secret/access
// keys, password or API-key hashes, or session tokens.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/auth"
	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/license"
	"github.com/openvtl/openvtld/internal/sysexec"
)

// ---------------------------------------------------- FC baseline ----------

// fcBaseline holds /sys/class/fc_host counter values captured once at
// daemon start, so fc.txt can present since-start deltas next to the raw
// since-boot counters — cold counters are unreadable without a "what was
// it when we started" reference.
var fcBaseline struct {
	at    time.Time
	stats string
}

// CaptureFCBaseline snapshots the FC host statistics; main() calls it
// once at startup. Read-only sysfs access, best-effort.
func CaptureFCBaseline() {
	fcBaseline.at = time.Now().UTC()
	fcBaseline.stats = readFCStats()
}

// readFCStats renders every readable counter under each FC host's
// statistics directory. Write-only entries (reset_statistics) and
// unreadable files are skipped silently.
func readFCStats() string {
	var b strings.Builder
	hosts, _ := filepath.Glob("/sys/class/fc_host/host*")
	sort.Strings(hosts)
	for _, h := range hosts {
		fmt.Fprintf(&b, "== %s\n", filepath.Base(h))
		files, _ := filepath.Glob(filepath.Join(h, "statistics", "*"))
		sort.Strings(files)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", filepath.Base(f), strings.TrimSpace(string(data)))
		}
	}
	if b.Len() == 0 {
		return "(no FC hosts)\n"
	}
	return b.String()
}

func (s *Server) supportBundle(w http.ResponseWriter, r *http.Request) {
	// GET is not admin-gated by requireAuth (reads are open to readonly);
	// this one is sensitive, so gate it explicitly.
	if u := sessionUser(r); u == nil || u.Role != auth.RoleAdmin {
		writeJSON(w, 403, map[string]string{"error": "admin role required"})
		return
	}
	ctx := r.Context()
	host, _ := os.Hostname()
	ts := time.Now().UTC()
	fname := fmt.Sprintf("openvtl-support-%s-%s.tar.gz", catalog.SanitizeName(host), ts.Format("20060102-150405"))

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	add := func(name string, data []byte) {
		hdr := &tar.Header{Name: "support/" + name, Mode: 0o600, Size: int64(len(data)), ModTime: ts}
		if err := tw.WriteHeader(hdr); err != nil {
			s.log.Warn("support bundle header", "file", name, "err", err)
			return
		}
		if _, err := tw.Write(data); err != nil {
			s.log.Warn("support bundle write", "file", name, "err", err)
		}
	}
	// sh runs a diagnostic command best-effort — a failure lands as text in
	// the file rather than aborting the whole bundle.
	sh := func(timeout time.Duration, name string, args ...string) []byte {
		out, err := sysexec.Run(ctx, timeout, name, args...)
		if err != nil {
			return []byte(out + "\n[command error: " + err.Error() + "]\n")
		}
		return []byte(out)
	}
	jsonOf := func(v any) []byte {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return []byte("[marshal error: " + err.Error() + "]")
		}
		return b
	}

	fp, _ := license.Compute()
	sysName, sysUUID, _ := s.db.SystemIdentity(ctx, catalog.SanitizeName(host))

	var mb strings.Builder
	fmt.Fprintf(&mb, "OpenVTL support bundle\n")
	fmt.Fprintf(&mb, "generated:   %s\n", ts.Format(time.RFC3339))
	fmt.Fprintf(&mb, "host:        %s\n", host)
	fmt.Fprintf(&mb, "version:     %s\n", s.version)
	fmt.Fprintf(&mb, "support key: %s\n", fp)
	fmt.Fprintf(&mb, "system:      %s (%s)\n", sysName, sysUUID)
	fmt.Fprintf(&mb, "\nRedacted: no secret/access keys, no password or API-key hashes, no database file.\n")
	add("manifest.txt", []byte(mb.String()))

	// Operational identity — the clone/multiplicity signal (feature #4).
	add("operational-identity.json", jsonOf(s.operationalIdentity(ctx, fp, sysName, sysUUID)))

	// Live status snapshot.
	add("status.json", jsonOf(s.inv.Snapshot()))

	// Storage.
	zpool := sh(10*time.Second, "zpool", "status")
	zpool = append(zpool, sh(10*time.Second, "zpool", "list", "-v")...)
	zpool = append(zpool, sh(10*time.Second, "zpool", "get", "all", "ovz")...)
	add("zpool.txt", zpool)
	add("zfs.txt", sh(10*time.Second, "zfs", "list", "-o", "name,used,logicalused,compressratio,referenced,mountpoint", "-r", "ovz"))

	// Logs (time- and line-capped).
	add("journal-openvtld.txt", sh(30*time.Second, "journalctl", "-u", "openvtld", "--since", "7 days ago", "--no-pager", "-n", "5000"))
	// At VERBOSE=3 the raw vtltape/vtllibrary stream is dominated by
	// hexdumps, element-status decodes and poll chatter — a flat 5000-line
	// cap delivered ~15 SECONDS of history while the filename implied 7
	// days. Filter that noise at collection so the line cap buys a real
	// time span, and keep a short raw tail so the most recent activity is
	// still available at full verbosity. The 500k-line pre-filter bound
	// keeps the pipeline inside the timeout on huge journals; the file's
	// own first/last timestamps state the window actually covered.
	add("journal-mhvtl.txt", sh(60*time.Second, "sh", "-c",
		`{ echo "# vtltape/vtllibrary, last 7 days, NOISE-FILTERED (hexdumps, element-status/poll machinery, routine host-poll decode removed), capped at 20000 lines."; `+
			`journalctl -t vtltape -t vtllibrary --since "7 days ago" --no-pager -n 500000 `+
			`| grep -avE 'dump_element_desc|decode_element_status|fill_element_status|fill_element_page|fill_ed\(|smc_read_element_status|num_available_elements|spc_mode_sense|lookup_mode_pg|spc_inquiry|lookup_log_pg|VX_TAPE_POLL_STATUS|readline\(\)|: ([0-9a-f]{2} ){4,}' `+
			`| tail -n 20000; }`))
	add("journal-mhvtl-tail-raw.txt", sh(30*time.Second, "journalctl", "-t", "vtltape", "-t", "vtllibrary", "--no-pager", "-n", "2000"))
	// Kernel log: the current boot in full (capped high), not a dmesg
	// ring tail — boot-time qla2xxx init (firmware load, port bring-up)
	// is exactly what scrolls out of a 2000-line tail on a chatty box.
	add("dmesg.txt", sh(15*time.Second, "sh", "-c",
		"journalctl -k -b --no-pager -n 20000 2>/dev/null || dmesg -T 2>/dev/null | tail -2000"))
	sctl := sh(10*time.Second, "systemctl", "status", "openvtld", "mhvtl.target", "--no-pager", "-l")
	sctl = append(sctl, sh(10*time.Second, "systemctl", "--failed", "--no-pager")...)
	add("systemctl.txt", sctl)
	ver := sh(5*time.Second, "uname", "-a")
	ver = append(ver, sh(5*time.Second, "sh", "-c", `echo "zfs=$(modinfo -F version zfs 2>/dev/null)"; echo "mhvtl=$(modinfo -F version mhvtl 2>/dev/null)"`)...)
	add("versions.txt", ver)

	// FC / qla2xxx state (v1.0 support-review finding): the current
	// hardware/kernel truth, not just openvtld's one-line digest — port
	// link state, live sessions (tgt_port_database is the ONLY
	// authoritative view, runbook §6), configfs as the kernel serves it
	// (drift vs intent is a historic failure class), target mode,
	// driver/firmware versions, sg mapping, and since-start counter
	// deltas. Everything read-only, best-effort.
	var fb strings.Builder
	sec := func(title string, data []byte) {
		fb.WriteString("### " + title + "\n")
		fb.Write(data)
		fb.WriteString("\n")
	}
	sec("fc_host ports (sysfs)", sh(10*time.Second, "sh", "-c",
		`for h in /sys/class/fc_host/host*; do [ -e "$h" ] || { echo "(no FC hosts)"; break; }; echo "== $h"; for f in port_name port_state port_type speed symbolic_name; do printf '%s: ' "$f"; cat "$h/$f" 2>/dev/null || echo '?'; done; done`))
	sec("fc_remote_ports (sysfs)", sh(10*time.Second, "sh", "-c",
		`for r in /sys/class/fc_remote_ports/rport-*; do [ -e "$r" ] || { echo "(none)"; break; }; echo "== $r"; for f in port_name port_state roles; do printf '%s: ' "$f"; cat "$r/$f" 2>/dev/null || echo '?'; done; done`))
	sec("qla2xxx tgt_port_database (live host sessions — authoritative)", sh(10*time.Second, "sh", "-c",
		`for d in /sys/kernel/debug/qla2xxx/*/tgt_port_database; do [ -e "$d" ] || { echo "(debugfs absent or no qla2xxx)"; break; }; echo "== $d"; cat "$d" 2>/dev/null; done`))
	sec("target configfs tree (kernel truth — compare against status.json intent)", sh(10*time.Second, "sh", "-c",
		`find /sys/kernel/config/target -maxdepth 6 2>/dev/null | sort || echo "(configfs absent)"`))
	sec("target mode", sh(5*time.Second, "sh", "-c",
		`printf 'qlini_mode: '; cat /sys/module/qla2xxx/parameters/qlini_mode 2>/dev/null || echo '?'; echo '-- /etc/modprobe.d/qla2xxx.conf:'; cat /etc/modprobe.d/qla2xxx.conf 2>/dev/null || echo '(absent)'`))
	sec("driver + firmware", sh(10*time.Second, "sh", "-c",
		`printf 'qla2xxx modinfo version: '; modinfo -F version qla2xxx 2>/dev/null || echo '?'; echo '-- /lib/firmware ql2* files:'; ls -l /lib/firmware/ql2* 2>/dev/null || echo '(none — see runbook §1.3a)'; printf 'firmware-qlogic package: '; dpkg-query -W firmware-qlogic 2>/dev/null || echo '(not installed)'`))
	sec("sg mapping (lsscsi -g)", sh(10*time.Second, "sh", "-c", "lsscsi -g 2>/dev/null || echo '(lsscsi unavailable)'"))
	sec("fc-built-epoch (daemon-generation stamp, runbook §1.2)", sh(5*time.Second, "sh", "-c",
		"cat /var/lib/openvtld/fc-built-epoch 2>/dev/null || echo '(absent)'"))
	if fcBaseline.at.IsZero() {
		sec("fc_host counters", []byte("(no baseline captured — daemon predates this feature?)\n"+readFCStats()))
	} else {
		sec(fmt.Sprintf("fc_host counters — BASELINE at daemon start (%s)", fcBaseline.at.Format(time.RFC3339)), []byte(fcBaseline.stats))
		sec(fmt.Sprintf("fc_host counters — CURRENT (%s); read as delta vs baseline", ts.Format(time.RFC3339)), []byte(readFCStats()))
	}
	add("fc.txt", []byte(fb.String()))

	// Config (non-secret).
	if b, err := os.ReadFile("/etc/mhvtl/device.conf"); err == nil {
		add("device.conf", b)
	}

	// DB extracts — curated + redacted.
	if ev, err := s.db.RecentEvents(ctx, 1000); err == nil {
		add("events.json", jsonOf(ev))
	}
	if au, err := s.db.RecentAudit(ctx, 1000); err == nil {
		add("audit.json", jsonOf(au))
	}
	if rs, err := s.db.ListRemotes(ctx); err == nil {
		for i := range rs {
			rs[i].AccessKey = "[redacted]" // SecretKey is json:"-"; blank access key too
			rs[i].SecretKey = ""
		}
		add("remotes.json", jsonOf(rs))
	}
	if set, err := s.db.AllSettings(ctx); err == nil {
		for k := range set {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "secret") || strings.Contains(lk, "password") ||
				strings.Contains(lk, "token") || strings.Contains(lk, "hash") {
				set[k] = "[redacted]"
			}
		}
		add("settings.json", jsonOf(set))
	}

	s.audit(r, "support.bundle", fname, nil)
}

// operationalIdentity is the high-entropy, per-instance history that a VM
// clone can't reproduce by pinning static IDs — support compares it across
// submissions to flag one support key running on multiple appliances.
func (s *Server) operationalIdentity(ctx context.Context, fp, sysName, sysUUID string) map[string]any {
	firstSeen := s.db.Setting(ctx, "system.first_seen", "")
	if firstSeen == "" {
		firstSeen = time.Now().UTC().Format(time.RFC3339)
		if err := s.db.SetSetting(ctx, "system.first_seen", firstSeen); err != nil {
			s.log.Warn("first_seen persist", "err", err)
		}
	}

	libSerials := []string{} // never nil — emit [] not null
	for _, l := range s.inv.Snapshot().Libraries {
		libSerials = append(libSerials, l.Library.Serial)
	}
	var jobsTotal, exportBytes int64
	if stats, err := s.db.JobStats(ctx); err == nil {
		for _, st := range stats {
			jobsTotal += st.Count
			if st.Kind == "export" && st.Outcome == "done" {
				exportBytes += st.Bytes
			}
		}
	}
	var cartCount int64
	if cc, err := s.db.CartStateCounts(ctx); err == nil {
		for _, c := range cc {
			cartCount += c.Count
		}
	}
	return map[string]any{
		"support_key":        fp,
		"system_name":        sysName,
		"system_uuid":        sysUUID,
		"first_seen":         firstSeen,
		"openvtld_version":   s.version,
		"openvtld_started":   s.started.UTC().Format(time.RFC3339),
		"zpool_guid":         strings.TrimSpace(shOut(ctx, "zpool", "get", "-H", "-o", "value", "guid", "ovz")),
		"zpool_created":      strings.TrimSpace(shOut(ctx, "zfs", "get", "-H", "-o", "value", "creation", "ovz")),
		"jobs_total":         jobsTotal,
		"export_bytes_total": exportBytes,
		"cartridges_total":   cartCount,
		"library_serials":    libSerials,
	}
}

func shOut(ctx context.Context, name string, args ...string) string {
	out, _ := sysexec.Run(ctx, 10*time.Second, name, args...)
	return out
}
