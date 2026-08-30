package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// MediaInfo is the on-disk footprint of one cartridge directory.
type MediaInfo struct {
	Label     string
	SizeBytes int64
	PhysBytes int64 // allocated on disk (post-compression), from st_blocks
	Modified  time.Time
}

// ScanMedia walks the pool directory. Cart dirs are DDNWxxL5-style
// (any name except lost+found and dotfiles); size is the sum of the
// media files (data.N, indx.N, meta.N, mam).
func ScanMedia(dir string) ([]MediaInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []MediaInfo
	for _, e := range ents {
		if !e.IsDir() || e.Name() == "lost+found" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		mi := MediaInfo{Label: e.Name()}
		files, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			info, err := f.Info()
			if err != nil {
				continue
			}
			mi.SizeBytes += info.Size()
			// st_blocks is always 512-byte units: on ZFS this is the
			// post-compression footprint (dedup savings are pool-wide
			// and not visible per-file) — the cart's "actual" size.
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				mi.PhysBytes += st.Blocks * 512
			}
			if info.ModTime().After(mi.Modified) {
				mi.Modified = info.ModTime()
			}
		}
		out = append(out, mi)
	}
	return out, nil
}
