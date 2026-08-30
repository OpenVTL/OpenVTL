package inventory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MhvtlLibrary is one library's declared topology from /etc/mhvtl.
// device.conf may declare libraries mhVTL isn't serving yet (a
// pending_restart library from the create wizard) — callers must
// tolerate a library with no live sg nodes.
type MhvtlLibrary struct {
	ID       int
	Target   int          // SCSI target on channel 0 (device.conf header)
	Serial   string
	Product  string
	HomeDir  string       // per-library "Home directory" (media root)
	Drives   []MhvtlDrive // sorted by Slot; index = UI drive index
	Barcodes []string     // declared cartridges from library_contents
}

type MhvtlDrive struct {
	QueueID   int // "Drive: 11" — the vtltape@NN instance
	Target    int // SCSI target on channel 0 (device.conf header)
	LibraryID int
	Slot      int
	Serial    string
	Product   string
}

var (
	reLibrary = regexp.MustCompile(`^Library:\s+(\d+)\s`)
	reDrive   = regexp.MustCompile(`^Drive:\s+(\d+)\s`)
	reTarget  = regexp.MustCompile(`\bTARGET:\s+(\d+)`)
	reField   = regexp.MustCompile(`^\s+([A-Za-z ]+):\s+(.+?)\s*$`)
	reSlot    = regexp.MustCompile(`^Slot\s+\d+:\s*(\S+)`)
	// " Library ID: 10 Slot: 01" — reField yields key "Library ID",
	// value "10 Slot: 01"; this splits the value.
	reLibSlot = regexp.MustCompile(`^(\d+)\s+Slot:\s+(\d+)`)
)

// ParseMhvtlConf reads device.conf and each library's
// library_contents.<id>. Drives attach to their library via the
// "Library ID:" field, never file order.
func ParseMhvtlConf(dir string) ([]MhvtlLibrary, error) {
	f, err := os.Open(filepath.Join(dir, "device.conf"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var libs []MhvtlLibrary
	var drives []MhvtlDrive
	var curLib *MhvtlLibrary
	var curDrv *MhvtlDrive

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := reLibrary.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			lib := MhvtlLibrary{ID: id}
			if t := reTarget.FindStringSubmatch(line); t != nil {
				lib.Target, _ = strconv.Atoi(t[1])
			}
			libs = append(libs, lib)
			curLib, curDrv = &libs[len(libs)-1], nil
			continue
		}
		if m := reDrive.FindStringSubmatch(line); m != nil {
			qid, _ := strconv.Atoi(m[1])
			drv := MhvtlDrive{QueueID: qid}
			if t := reTarget.FindStringSubmatch(line); t != nil {
				drv.Target, _ = strconv.Atoi(t[1])
			}
			drives = append(drives, drv)
			curDrv, curLib = &drives[len(drives)-1], nil
			continue
		}
		m := reField.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, val := strings.TrimSpace(m[1]), m[2]
		switch {
		case curDrv != nil && key == "Unit serial number":
			curDrv.Serial = val
		case curDrv != nil && key == "Product identification":
			curDrv.Product = val
		case curDrv != nil && key == "Library ID":
			if lm := reLibSlot.FindStringSubmatch(val); lm != nil {
				curDrv.LibraryID, _ = strconv.Atoi(lm[1])
				curDrv.Slot, _ = strconv.Atoi(lm[2])
			}
		case curLib != nil && key == "Unit serial number":
			curLib.Serial = val
		case curLib != nil && key == "Product identification":
			curLib.Product = val
		case curLib != nil && key == "Home directory":
			curLib.HomeDir = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Zero libraries is a valid state: a fresh install, or mid
	// storage-re-foundation before the wizard declares the first
	// library. Callers surface "nothing to serve" themselves.

	for i := range libs {
		lib := &libs[i]
		for _, d := range drives {
			if d.LibraryID == lib.ID {
				lib.Drives = append(lib.Drives, d)
			}
		}
		sort.Slice(lib.Drives, func(a, b int) bool { return lib.Drives[a].Slot < lib.Drives[b].Slot })

		// library_contents.<id> — declared barcodes (informational; live
		// truth comes from the changer).
		lc, err := os.Open(filepath.Join(dir, fmt.Sprintf("library_contents.%d", lib.ID)))
		if err == nil {
			sc2 := bufio.NewScanner(lc)
			for sc2.Scan() {
				if m := reSlot.FindStringSubmatch(strings.TrimSpace(sc2.Text())); m != nil {
					lib.Barcodes = append(lib.Barcodes, m[1])
				}
			}
			lc.Close()
		}
	}
	sort.Slice(libs, func(a, b int) bool { return libs[a].ID < libs[b].ID })
	return libs, nil
}
