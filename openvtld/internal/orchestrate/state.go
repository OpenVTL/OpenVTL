package orchestrate

// Read-only state for the Targets & ACLs view (v0.5; multi-port +
// registry shaped in v0.7). configfs is structure; live FC login state
// comes from the qla2xxx debugfs port database (target-mode sessions
// never appear in fc_remote_ports/targetcli — v0.5 field lesson).
// Everything here is a read; nothing touches the targets.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/store"
)

// PortState is a local HBA port (fc_host class).
type PortState struct {
	Host  string `json:"host"`
	WWPN  string `json:"wwpn"`
	State string `json:"state"`
	Speed string `json:"speed"`
}

// PortView adds serving/built to a port for the Targets page.
type PortView struct {
	PortState
	Serving bool `json:"serving"`
	Built   bool `json:"built"`
}

// LUNView is one row of the (port-identical) LUN table.
type LUNView struct {
	LUN       int    `json:"lun"`
	Backstore string `json:"backstore"`
	Device    string `json:"device"`
	Library   int    `json:"library"`
}

// InitiatorView is one registry row joined with live state.
type InitiatorView struct {
	store.InitiatorACL
	LoggedIn     bool   `json:"logged_in"`
	PortStateStr string `json:"port_state,omitempty"`
	Applied      bool   `json:"applied"` // present everywhere its scope says it should be
}

const sysFCRemote = "/sys/class/fc_remote_ports"
const sysFCHost = "/sys/class/fc_host"
const debugfsQla = "/sys/kernel/debug/qla2xxx"

// FirmwareWarning reports the missing-qla2xxx-firmware hazard: the HBA
// boots fine on its flash-resident firmware, but ANY ISP reset (target
// create/delete, error recovery) re-loads firmware from /lib/firmware —
// when it's absent the port is dead until reboot ("Failed to load
// firmware image", alloc_iocbs failures; observed live: a rebuild
// wedged and every port went Offline/Linkdown). Checking one family
// file is enough — the firmware-qlogic package ships them all.
func FirmwareWarning() string {
	if len(hostPorts()) == 0 {
		return ""
	}
	for _, fw := range []string{"ql2400_fw.bin", "ql2500_fw.bin", "ql2700_fw.bin"} {
		if _, err := os.Stat("/lib/firmware/" + fw); err == nil {
			return ""
		}
	}
	return "qla2xxx firmware missing from /lib/firmware (install firmware-qlogic) — FC ports cannot survive a reset without it"
}

// colonsToNaa is the inverse of naaToColons: "51:40:..." -> "naa.5140...".
func colonsToNaa(c string) string {
	return "naa." + strings.ReplaceAll(strings.ToLower(c), ":", "")
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// remotePorts returns logged-in FC initiator WWPNs (naa. form) -> a
// short state string, from the driver's debugfs port database (+ any
// fc_remote_ports for future initiator-mode use).
func remotePorts() map[string]string {
	out := map[string]string{}
	dbs, _ := filepath.Glob(filepath.Join(debugfsQla, "qla2xxx_*", "tgt_port_database"))
	wwpnRe := regexp.MustCompile(`(?:[0-9a-f]{2}:){7}[0-9a-f]{2}`)
	for _, db := range dbs {
		b, err := os.ReadFile(db)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.ToLower(string(b)), "\n") {
			if m := wwpnRe.FindString(line); m != "" {
				out[colonsToNaa(m)] = "nexus up"
			}
		}
	}
	entries, _ := filepath.Glob(filepath.Join(sysFCRemote, "rport-*"))
	for _, r := range entries {
		name := readTrim(filepath.Join(r, "port_name")) // "0x2100000e1e123456"
		if !strings.HasPrefix(name, "0x") {
			continue
		}
		out["naa."+strings.TrimPrefix(name, "0x")] = readTrim(filepath.Join(r, "port_state"))
	}
	return out
}

func hostPorts() []PortState {
	out := []PortState{} // never nil — a null array once black-screened the UI (v0.6)
	hosts, _ := filepath.Glob(filepath.Join(sysFCHost, "host*"))
	sort.Strings(hosts)
	for _, h := range hosts {
		name := readTrim(filepath.Join(h, "port_name"))
		if name == "" {
			continue
		}
		out = append(out, PortState{
			Host:  filepath.Base(h),
			WWPN:  "naa." + strings.TrimPrefix(name, "0x"),
			State: readTrim(filepath.Join(h, "port_state")),
			Speed: readTrim(filepath.Join(h, "speed")),
		})
	}
	return out
}

// lunNumber parses "lun_3" -> 3.
func lunNumber(name string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(name, "lun_"))
	return n, err == nil && strings.HasPrefix(name, "lun_")
}

// PortsView lists every FC port with serving/built state.
func (f *FC) PortsView(ctx context.Context) []PortView {
	dis := f.disabledPorts(ctx)
	out := []PortView{}
	for _, p := range hostPorts() {
		out = append(out, PortView{
			PortState: p,
			Serving:   !dis[p.WWPN],
			Built:     fcTargetBuilt(p.WWPN),
		})
	}
	return out
}

// LUNTable is the design-intent LUN table (identical on every port),
// serial-verified against live sg discovery.
func (f *FC) LUNTable(ctx context.Context, libs []inventory.MhvtlLibrary) ([]LUNView, error) {
	want, err := f.expected(ctx, libs)
	if err != nil {
		return []LUNView{}, err
	}
	out := make([]LUNView, 0, len(want))
	for lun, b := range want {
		out = append(out, LUNView{LUN: lun, Backstore: b.name, Device: b.dev, Library: b.lib})
	}
	return out, nil
}

// Initiators joins the registry with live login + applied state.
func (f *FC) Initiators(ctx context.Context) []InitiatorView {
	rows, err := f.db.ListACLs(ctx, "")
	if err != nil {
		return []InitiatorView{}
	}
	fcLogged := remotePorts()
	ports := f.ServingPorts(ctx)

	out := []InitiatorView{}
	for _, r := range rows {
		if r.Fabric != store.FabricFC {
			continue // stray non-FC rows from pre-FC-only builds are inert
		}
		v := InitiatorView{InitiatorACL: r, Applied: true}
		if ps, ok := fcLogged[r.WWPN]; ok {
			v.LoggedIn, v.PortStateStr = true, ps
		}
		for _, p := range ports {
			if fcInScope(p.WWPN)(r) && fcTargetBuilt(p.WWPN) {
				if _, err := os.Stat(filepath.Join(fcTPG(p.WWPN).dir, naaToColons(r.WWPN))); err != nil {
					v.Applied = false
				}
			}
		}
		if len(ports) == 0 && r.Ports != store.ScopeNone {
			v.Applied = false // nothing to be applied to (yet)
		}
		out = append(out, v)
	}
	return out
}

// UnmanagedACLs lists configfs ACLs that are not in the registry —
// drift we surface but never delete.
func (f *FC) UnmanagedACLs(ctx context.Context) []string {
	rows, _ := f.db.ListACLs(ctx, "")
	known := map[string]bool{}
	for _, r := range rows {
		known[r.WWPN] = true
	}
	out := []string{}
	for _, p := range hostPorts() {
		dir := fcTPG(p.WWPN).dir
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && !known[colonsToNaa(e.Name())] {
				out = append(out, p.WWPN+": "+colonsToNaa(e.Name()))
			}
		}
	}
	return out
}
