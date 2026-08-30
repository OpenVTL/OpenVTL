package inventory

// Library removal (v0.7 operator ask): excise a library AND its
// drives from device.conf — drives never leave independently
// (operator decision: no per-drive modification). mhVTL only reads
// the config at daemon start, so a live library keeps serving until
// the data-plane restart the API layer runs right after; a pending
// one just disappears.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
)

// ErrNotDeclared: the library is not in device.conf — a retried
// deletion after a partial failure sees this and continues.
var ErrNotDeclared = errors.New("library not declared in device.conf")

// RemoveLibrary rewrites device.conf without the library's block and
// every drive block whose "Library ID" matches, and removes the
// per-instance systemd drop-in dirs. library_contents.<id> is left
// for RemoveLibraryContents (a live removal regenerates it once more
// during the Apply contents rewrite).
func (e *Engine) RemoveLibrary(ctx context.Context, libID int) error {
	// Drive queue ids first — needed for the drop-in dirs.
	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err != nil {
		return fmt.Errorf("mhvtl config: %w", err)
	}
	var queues []int
	found := false
	for _, l := range libs {
		if l.ID == libID {
			found = true
			for _, d := range l.Drives {
				queues = append(queues, d.QueueID)
			}
		}
	}
	if !found {
		return fmt.Errorf("library %d: %w", libID, ErrNotDeclared)
	}

	confPath := filepath.Join(e.cfg.MhvtlConf, "device.conf")
	orig, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	if backup := confPath + ".bak-openvtl"; !fileExists(backup) {
		_ = os.WriteFile(backup, orig, 0o644)
	}

	// Block-wise filter: blocks start at ^Library:/^Drive: lines; the
	// preamble is kept verbatim. A Drive block's fate is decided by its
	// "Library ID:" field, so blocks are buffered until the next start.
	idStr := strconv.Itoa(libID)
	var out, buf []string
	dropCur, inBlock := false, false
	flush := func() {
		if !dropCur {
			out = append(out, buf...)
		}
		buf = nil
	}
	for _, line := range strings.Split(string(orig), "\n") {
		if m := reLibrary.FindStringSubmatch(line); m != nil {
			flush()
			inBlock, dropCur = true, m[1] == idStr
			buf = append(buf, line)
			continue
		}
		if reDrive.MatchString(line) {
			flush()
			inBlock, dropCur = true, false
			buf = append(buf, line)
			continue
		}
		if !inBlock {
			out = append(out, line)
			continue
		}
		if m := reField.FindStringSubmatch(line); m != nil && strings.TrimSpace(m[1]) == "Library ID" {
			if lm := reLibSlot.FindStringSubmatch(m[2]); lm != nil && lm[1] == idStr {
				dropCur = true
			}
		}
		buf = append(buf, line)
	}
	flush()

	tmp := confPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, confPath); err != nil {
		return err
	}

	// Per-instance drop-ins; daemon-reload so mhVTL's generator forgets
	// the instances (the live-case Apply reloads again — harmless).
	dirs := []string{fmt.Sprintf("%s/vtllibrary@%d.service.d", systemdDir, libID)}
	for _, q := range queues {
		dirs = append(dirs, fmt.Sprintf("%s/vtltape@%d.service.d", systemdDir, q))
	}
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
	_, _ = sysexec.Run(ctx, 30*time.Second, "systemctl", "daemon-reload")

	e.log.Info("library removed from device.conf", "library", libID, "drives", len(queues))
	return nil
}

// RemoveLibraryContents deletes library_contents.<id> — called after
// any restart sequence so nothing regenerates it.
func (e *Engine) RemoveLibraryContents(libID int) {
	_ = os.Remove(filepath.Join(e.cfg.MhvtlConf, fmt.Sprintf("library_contents.%d", libID)))
}

// PurgeMedia removes the media directories for the given labels under
// home — cascade library deletion, AFTER the library stopped serving
// (no MAP transit needed: the whole changer is gone). Only dirs that
// look like mhVTL media (mam or eviction stub) are touched.
// progress (nil ok) is called after each cartridge is handled — freeing
// a big cart's blocks on a dedup pool takes real time (DDT updates per
// freed block; observed minutes for tens of carts), and the operator
// must see that the delete is alive, not wedged.
func (e *Engine) PurgeMedia(home string, labels []string, progress func(label string, done, total int)) (removed int, skipped []string) {
	for i, l := range labels {
		dir := filepath.Join(home, l)
		if _, err := os.Stat(dir); err == nil {
			if !fileExists(filepath.Join(dir, "mam")) && !fileExists(filepath.Join(dir, ".openvtl-evicted.json")) {
				skipped = append(skipped, l)
			} else if err := os.RemoveAll(dir); err != nil {
				e.log.Warn("cart media purge", "label", l, "err", err)
				skipped = append(skipped, l)
			} else {
				removed++
			}
		}
		if progress != nil {
			progress(l, i+1, len(labels))
		}
	}
	return removed, skipped
}
