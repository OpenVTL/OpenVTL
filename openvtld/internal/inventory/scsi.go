package inventory

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
)

// SGDevice is one row of lsscsi -g, identity-verified via sg_inq.
// sg nodes renumber on every boot AND on HBA reprobe — nothing may cache
// them; discovery re-derives and serial-verifies on every use that matters.
type SGDevice struct {
	Type    string // mediumx | tape
	Product string
	Serial  string
	SG      string // /dev/sgN
	Aux     string // /dev/sch0 or /dev/stN
}

var reLsscsi = regexp.MustCompile(`^\[[\d:]+\]\s+(\S+)\s+(\S+)\s+(.{1,16}?)\s{2,}\S+\s+(\S+)\s+(\S+)\s*$`)

// DiscoverSG lists changer/tape devices and reads each unit serial.
func DiscoverSG(ctx context.Context) ([]SGDevice, error) {
	out, err := sysexec.Run(ctx, 10*time.Second, "lsscsi", "-g")
	if err != nil {
		return nil, err
	}
	var devs []SGDevice
	for _, line := range strings.Split(out, "\n") {
		m := reLsscsi.FindStringSubmatch(strings.TrimRight(line, " "))
		if m == nil {
			continue
		}
		typ := m[1]
		if typ != "mediumx" && typ != "tape" {
			continue
		}
		d := SGDevice{Type: typ, Product: strings.TrimSpace(m[3]), Aux: m[4], SG: m[5]}
		d.Serial = inqSerial(ctx, d.SG)
		devs = append(devs, d)
	}
	// Zero devices is a valid state (mhVTL stopped, or no libraries
	// declared yet) — callers decide whether that's fatal.
	return devs, nil
}

// NOTE (2026-07-05): removing stale mhVTL SCSI devices via the generic
// sysfs delete (echo 1 > .../delete) was tried to clear the delete-a-
// live-library discovery wedge without a reboot. It CORRUPTS mhVTL's
// kernel LU list — the generic delete bypasses mhVTL's own bookkeeping,
// so the next add_lu_store on daemon restart trips list-add corruption
// and oopses the box (observed kernel panic in add_lu_store [mhvtl] ->
// __list_add_valid_or_report; vtllibrary died holding a lock). Never do
// this. The safe path for reducing the served set is a reboot: deleting
// a live library removes its config + media + rows, then reboots, and
// the fresh boot simply never re-creates the removed devices.

// SerialMatches reports whether a discovered device serial corresponds
// to a device.conf serial. Normally strict equality, but the 3584
// changer personality presents its serial per GA32-0454-00 (mhVTL
// patches 12/13):
// INQUIRY bytes 38-49 are right-justified with leading ASCII zeroes,
// and VPD page 0x80's 0x10-byte payload appends the 4-hex-digit First
// Storage Element Address after the 12-byte serial — sg_vpd reads the
// whole payload as the serial. So conf serial OVTL409366 is discovered
// as "00OVTL4093660400" (a real 3584 presents exactly the same shape;
// its all-numeric serials just make the padding invisible). Accept the
// zero-padded and address-suffixed forms.
func SerialMatches(dev, conf string) bool {
	if dev == conf {
		return true
	}
	if conf == "" || len(dev) < len(conf) {
		return false
	}
	d := dev
	for len(d) > len(conf) && d[0] == '0' {
		d = d[1:]
	}
	if d == conf {
		return true
	}
	return len(d) == len(conf)+4 && strings.HasPrefix(d, conf)
}

// sg_vpd prints "Product serial number:" (older builds: "Unit serial number:").
var reSerial = regexp.MustCompile(`(?:Unit|Product) serial number:\s*(\S+)`)

func inqSerial(ctx context.Context, dev string) string {
	// Serial lives in VPD page 0x80. sg_vpd decodes it; sg_inq -p 0x80
	// only hex-dumps the page on Debian 13's sg3-utils.
	out, err := sysexec.Run(ctx, 5*time.Second, "sg_vpd", "-p", "sn", dev)
	if err != nil {
		return ""
	}
	if m := reSerial.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// --- changer element status (mtx) ---

type MtxStatus struct {
	Drives []MtxDrive
	Slots  []MtxSlot
}

type MtxDrive struct {
	Num    int // mtx drive number (0-based)
	Label  string
	Source int // "(Storage Element N Loaded)" — home slot, 0 if unknown
}

type MtxSlot struct {
	Num   int // mtx storage-element number (1-based)
	IE    bool
	Label string
}

var (
	reDTE  = regexp.MustCompile(`^Data Transfer Element (\d+):(Empty|Full)`)
	reDTES = regexp.MustCompile(`\(Storage Element (\d+) Loaded\)`)
	reDTEV = regexp.MustCompile(`VolumeTag\s*=\s*(\S+)`)
	// mtx prints "Full :VolumeTag=X" (space before colon) for slots.
	reSE = regexp.MustCompile(`^\s*Storage Element (\d+)\s*(IMPORT/EXPORT)?:(Empty|Full)(?:\s*:VolumeTag\s*=\s*(\S+))?`)
)

// MoveCart issues an mtx transfer between element numbers (storage and
// IE share mtx's element numbering, so this covers unvaulting IE->slot).
func MoveCart(ctx context.Context, changerSG string, from, to int) error {
	_, err := sysexec.Run(ctx, 60*time.Second, "mtx", "-f", changerSG,
		"transfer", strconv.Itoa(from), strconv.Itoa(to))
	if err != nil {
		return fmt.Errorf("mtx transfer %d->%d: %w", from, to, err)
	}
	return nil
}

// LoadDrive moves a cart from a slot element into a drive (mtx load).
func LoadDrive(ctx context.Context, changerSG string, slot, drive int) error {
	_, err := sysexec.Run(ctx, 60*time.Second, "mtx", "-f", changerSG,
		"load", strconv.Itoa(slot), strconv.Itoa(drive))
	if err != nil {
		return fmt.Errorf("mtx load slot %d -> drive %d: %w", slot, drive, err)
	}
	return nil
}

// UnloadDrive moves a drive's cart back to a slot element (mtx unload).
func UnloadDrive(ctx context.Context, changerSG string, slot, drive int) error {
	_, err := sysexec.Run(ctx, 60*time.Second, "mtx", "-f", changerSG,
		"unload", strconv.Itoa(slot), strconv.Itoa(drive))
	if err != nil {
		return fmt.Errorf("mtx unload drive %d -> slot %d: %w", drive, slot, err)
	}
	return nil
}

// PollChanger runs mtx status against the changer sg node.
func PollChanger(ctx context.Context, changerSG string) (*MtxStatus, error) {
	out, err := sysexec.Run(ctx, 15*time.Second, "mtx", "-f", changerSG, "status")
	if err != nil {
		return nil, err
	}
	st := &MtxStatus{}
	for _, line := range strings.Split(out, "\n") {
		if m := reDTE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			d := MtxDrive{Num: n}
			if m[2] == "Full" {
				if v := reDTEV.FindStringSubmatch(line); v != nil {
					d.Label = strings.TrimSpace(v[1])
				}
				if sm := reDTES.FindStringSubmatch(line); sm != nil {
					d.Source, _ = strconv.Atoi(sm[1])
				}
			}
			st.Drives = append(st.Drives, d)
			continue
		}
		if m := reSE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			s := MtxSlot{Num: n, IE: m[2] != ""}
			if m[3] == "Full" && m[4] != "" {
				s.Label = strings.TrimSpace(m[4])
			}
			st.Slots = append(st.Slots, s)
		}
	}
	return st, nil
}
