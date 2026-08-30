package inventory

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
	"golang.org/x/sys/unix"
)

// CollectPoolStats gathers one pool's usage from ZFS. dataset is
// "<zpool>/<name>" (the OpenVTL pool is a dataset in the single system
// zpool). Filesystem numbers come from statfs (physical, post-
// compression); the dedupe ratio is pool-wide (the shared dedup vdev
// serves every dataset) and compression is per-dataset.
func CollectPoolStats(ctx context.Context, mount, dataset, _ string) (PoolStats, error) {
	ps := PoolStats{CollectedAt: time.Now().UTC()}

	var st unix.Statfs_t
	if err := unix.Statfs(mount, &st); err == nil {
		bs := int64(st.Bsize)
		ps.FSTotalBytes = int64(st.Blocks) * bs
		ps.FSUsedBytes = int64(st.Blocks-st.Bfree) * bs
		ps.VDOPhysBytes = ps.FSTotalBytes
	}

	// Physical used + logical (pre-dedup/compress) for the space-saving
	// ratio, plus the dedupe granularity (recordsize, RAM-scaled at pool
	// creation since v0.9).
	if out, err := sysexec.Run(ctx, 10*time.Second, "zfs", "get", "-Hp", "-o", "value",
		"used,logicalused,recordsize", dataset); err == nil {
		f := strings.Fields(out)
		if len(f) >= 3 {
			used, _ := strconv.ParseInt(f[0], 10, 64)
			logical, _ := strconv.ParseInt(f[1], 10, 64)
			ps.VDOUsedBytes = used
			ps.LogicalBytes = logical
			ps.RecordBytes, _ = strconv.ParseInt(f[2], 10, 64)
			if logical > 0 && used > 0 {
				ps.VDOSavingPct = int((1 - float64(used)/float64(logical)) * 100)
			}
		}
	}
	if out, err := sysexec.Run(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value",
		"compressratio", dataset); err == nil {
		ps.CompressRatio, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(out), "x"), 64)
	}
	// Pool-wide truth: dedup is GLOBAL (the shared dedup vdev serves
	// every dataset), so dataset `used` is post-compression only —
	// dedup savings exist exclusively at zpool level. At 9× dedup the
	// dataset `used` read 4.7T on a 3.14T pool (v0.9 30-save study), so
	// anything treating it as "physical" was wildly wrong.
	zpool := dataset
	if i := strings.IndexByte(zpool, '/'); i >= 0 {
		zpool = zpool[:i]
	}
	if out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "get", "-H", "-o", "value",
		"dedupratio", zpool); err == nil {
		ps.DedupRatio, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(out), "x"), 64)
	}
	if out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "list", "-Hp", "-o",
		"size,alloc", zpool); err == nil {
		if f := strings.Fields(out); len(f) >= 2 {
			ps.ZpoolSizeBytes, _ = strconv.ParseInt(f[0], 10, 64)
			ps.ZpoolAllocBytes, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	// Per-pool share of real disk ≈ used ÷ global dedupratio (exact
	// when one pool dominates; the only honest per-dataset attribution
	// ZFS allows).
	ps.PhysEstBytes = ps.VDOUsedBytes
	if ps.DedupRatio > 1 {
		ps.PhysEstBytes = int64(float64(ps.VDOUsedBytes) / ps.DedupRatio)
	}
	return ps, nil
}
