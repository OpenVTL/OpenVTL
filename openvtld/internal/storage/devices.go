// Package storage is the pool builder's front end: block-device
// enumeration with hard eligibility rails, feeding the ZFS system-pool
// builder (per-pool, job-tracked).
//
// The one safety invariant: a device that holds anything — filesystem
// or partition signature, mountpoint anywhere in its tree, LVM PV —
// is never eligible and the API offers no force path. Destroying data
// requires the operator to wipe the disk out-of-band first.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
)

// BlockDevice is one whole disk from lsblk, annotated with eligibility
// and its OpenVTL role if it has one.
type BlockDevice struct {
	Path       string `json:"path"` // /dev/sdX — renumbers across boots, resolve ByID for persistence
	ByID       string `json:"by_id,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	Model      string `json:"model,omitempty"`
	Transport  string `json:"transport,omitempty"`
	Rotational bool   `json:"rotational"`
	Eligible   bool   `json:"eligible"`
	Reason     string `json:"reason,omitempty"` // why not eligible
	Role       string `json:"role,omitempty"`   // os | cache-device | pool:<name> | ""
}

// lsblk -J node
type lsblkDev struct {
	Path       string     `json:"path"`
	Type       string     `json:"type"`
	Size       int64      `json:"size"`
	FSType     *string    `json:"fstype"`
	PTType     *string    `json:"pttype"`
	Mountpoint *string    `json:"mountpoint"`
	Model      *string    `json:"model"`
	Tran       *string    `json:"tran"`
	Rota       bool       `json:"rota"`
	Children   []lsblkDev `json:"children"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// treeState walks a device subtree for anything that makes it
// untouchable.
func treeState(d lsblkDev) (hasSignature bool, sig string, mounted bool, mnt string, hasRoot bool) {
	if s := str(d.FSType); s != "" {
		hasSignature, sig = true, s
	}
	if s := str(d.PTType); s != "" && !hasSignature {
		hasSignature, sig = true, s+" partition table"
	}
	if m := str(d.Mountpoint); m != "" {
		mounted, mnt = true, m
		if m == "/" || strings.HasPrefix(m, "/boot") {
			hasRoot = true
		}
	}
	for _, c := range d.Children {
		cs, csig, cm, cmnt, cr := treeState(c)
		if cs && !hasSignature {
			hasSignature, sig = true, csig
		}
		if cm && !mounted {
			mounted, mnt = true, cmnt
		}
		hasRoot = hasRoot || cr
	}
	return
}

// findByID resolves a /dev/sdX path to a stable /dev/disk/by-id
// symlink (wwn- preferred, then scsi-/ata-/nvme-, else any non-part).
func findByID(dev string) string {
	links, _ := filepath.Glob("/dev/disk/by-id/*")
	best := ""
	for _, l := range links {
		if strings.Contains(filepath.Base(l), "-part") {
			continue
		}
		t, err := os.Readlink(l)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(t) {
			t = filepath.Clean(filepath.Join(filepath.Dir(l), t))
		}
		if t != dev {
			continue
		}
		name := filepath.Base(l)
		switch {
		case strings.HasPrefix(name, "wwn-"):
			return l
		case best == "" || strings.HasPrefix(name, "scsi-") || strings.HasPrefix(name, "nvme-") || strings.HasPrefix(name, "ata-"):
			best = l
		}
	}
	return best
}

// resolveDev follows a stored by-id (or plain) path to today's /dev
// node.
func resolveDev(path string) (string, error) {
	t, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("device %s not present: %w", path, err)
	}
	return t, nil
}

// roles annotates devices already spoken for. Keyed by resolved /dev
// path.
type roleMap map[string]string

// Devices enumerates whole disks with eligibility + role. cacheDev and
// poolDevs come from persisted state (by-id paths / recorded devs).
func Devices(ctx context.Context, roles roleMap) ([]BlockDevice, error) {
	// --tree is required: this util-linux emits FLAT json without it,
	// and the eligibility walk would never see partition mounts.
	out, err := sysexec.Run(ctx, 15*time.Second, "lsblk", "-J", "-b", "--tree",
		"-o", "PATH,TYPE,SIZE,FSTYPE,PTTYPE,MOUNTPOINT,MODEL,TRAN,ROTA")
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var tree struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		return nil, fmt.Errorf("lsblk parse: %w", err)
	}

	var devs []BlockDevice
	for _, d := range tree.Blockdevices {
		if d.Type != "disk" {
			continue
		}
		bd := BlockDevice{
			Path: d.Path, SizeBytes: d.Size, Model: str(d.Model),
			Transport: str(d.Tran), Rotational: d.Rota,
			ByID: findByID(d.Path),
		}
		hasSig, sig, mounted, mnt, hasRoot := treeState(d)
		switch {
		case hasRoot:
			bd.Role, bd.Reason = "os", "operating system disk"
		case roles[d.Path] != "":
			bd.Role = roles[d.Path]
			bd.Reason = "in use as " + bd.Role
		case mounted:
			bd.Reason = "mounted at " + mnt
		case hasSig:
			bd.Reason = "carries a " + sig + " signature — wipe out-of-band to reuse"
		default:
			bd.Eligible = true
		}
		devs = append(devs, bd)
	}
	return devs, nil
}

// VerifyBare re-checks a single device immediately before destructive
// use (the TOCTOU guard): wipefs -n must report nothing and lsblk must
// show no children/mounts.
func VerifyBare(ctx context.Context, dev string) error {
	out, err := sysexec.Run(ctx, 10*time.Second, "wipefs", "-n", dev)
	if err != nil {
		return fmt.Errorf("wipefs probe %s: %w", dev, err)
	}
	// wipefs -n prints a table when signatures exist, nothing otherwise.
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("device %s grew a signature since enumeration:\n%s", dev, s)
	}
	devs, err := Devices(ctx, nil)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.Path == dev {
			if !d.Eligible {
				return fmt.Errorf("device %s no longer eligible: %s", dev, d.Reason)
			}
			return nil
		}
	}
	return fmt.Errorf("device %s not found as a whole disk", dev)
}
