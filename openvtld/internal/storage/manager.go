package storage

// Manager owns the ZFS storage plane: one-time system-pool setup (data
// vdev(s) on HDD + one SSD dedup vdev — dedupe metadata on the fast
// disk) and per-pool datasets. One mutation runs at a time.
//
// Model: ONE system zpool holds every pool as a dataset with dedup=on,
// so deduplication is GLOBAL across all pools/libraries (the single SSD
// dedup vdev serves the whole system). Each OpenVTL "pool" is a dataset
// <zpool>/<name> mounted at /var/lib/openvtl/pools/<name> via
// legacy+fstab, so mhVTL's RequiresMountsFor drop-ins order correctly at
// boot. Compression is zstd (since v0.9; lz4 on pools built earlier);
// per-cart mhVTL LZO stays off (v0.2). ARC is
// capped so dedup-table lookups lean on the SSD dedup vdev, leaving RAM
// for mhVTL/openvtld on a 4GB appliance.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openvtl/openvtld/internal/events"
	"github.com/openvtl/openvtld/internal/store"
	"github.com/openvtl/openvtld/internal/sysexec"
)

// Zpool is the single system pool; every OpenVTL pool is a dataset in it.
const Zpool = "ovz"

const PoolsRoot = "/var/lib/openvtl/pools"

// totalRAMBytes reads MemTotal from /proc/meminfo (0 when unreadable —
// callers fall back to the smallest tier / floor value).
func totalRAMBytes() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

// arcMaxBytes gives the ARC everything except a reserve for the OS, the
// mhVTL daemons and openvtld: reserve = MemTotal/4 but never under 2GB,
// so a 4GB reference box keeps 2GB and larger boxes hand the ARC 75%.
// Floored at 512MB (proven 2026-07-05 on the 4GB box: 512MB ARC →
// ~290MB/s dedup with the DDT on the SSD vdev).
//
// The ARC has to hold the dedup table or the box melts down. Measured on
// a test system mid-scrub: 3.1T pool at 16K records → DDT of 56.3M
// entries, 18.1G in core, against the old 25%-of-RAM cap of 7.8G. The DDT
// evicted every data buffer (data_size literally 0) and still didn't fit,
// so arc_evict spun at 93% of a core for 2.5 days — 99.6 BILLION
// evict_skips, 11.2M evict_not_enough — re-reading DDT entries it had just
// dropped, while 21G of RAM sat free. Raising the cap to 23G stopped it
// dead: evict_skip froze, arc_evict went to 0%.
//
// So the old 16GB ceiling is gone: it capped the ARC below the DDT of any
// large pool, which is exactly when the ARC matters most. Note the DDT
// scales with pool size and recordsize, NOT with RAM, so no fraction of
// RAM can guarantee a fit — recordSizeFromRAM below hands a big-RAM box
// 16K records, which maximises DDT entries and can still outgrow the ARC
// on a large enough pool. dedup_table_quota=auto is the backstop.
func arcMaxBytes() int64 {
	mem := totalRAMBytes()
	reserve := mem / 4
	if reserve < 2<<30 {
		reserve = 2 << 30
	}
	arc := mem - reserve
	if arc < 512<<20 {
		arc = 512 << 20
	}
	return arc
}

// recordSizeFromRAM picks a pool's dedupe granularity from MemTotal at
// pool-creation time. Smaller records
// catch more duplicate data (identical full-system saves: 1.0× dedup at
// 1M, 1.28× at 16K, ~1.9× ceiling; repeated library saves hit ~10× at
// 16K vs ~1.7× at 1M) but cost DDT RAM (~212B in-core per unique block,
// measured) and compression (zstd 1.46× @1M → 1.22× @16K on a real save
// stream). Floor is 16K: the measured dedup cliff sits at 16K→32K (IBM
// i writes ~16K tape blocks, so shifts between saves are 16K-granular)
// and ashift=12 makes finer records trade compression for nothing
// provable. The choice is stamped on the dataset at creation — adding
// RAM later gives NEW pools finer granularity; existing pools keep
// theirs (recordsize only affects new writes anyway).
func recordSizeFromRAM() string {
	switch ram := totalRAMBytes(); {
	case ram >= 24<<30:
		return "16K"
	case ram >= 12<<30:
		return "32K"
	case ram >= 6<<30:
		return "64K"
	default:
		return "128K"
	}
}

// dedupDeviceKey persists the by-id of the permanent dedupe (metadata)
// device. It is chosen once at first setup and never changed: ZFS cannot
// relocate or remove a dedup vdev, so the SSD is the system's fixed
// metadata home. It survives pool teardown (remembered, re-added on the
// next setup) so the operator only re-picks a data disk — the "locked in"
// dedupe device the operator asked for.
const dedupDeviceKey = "storage.dedup_device"

// poolNameRe keeps dataset leaf names simple (a-z0-9_).
var poolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,23}$`)

type Manager struct {
	db  *store.Store
	bus *events.Bus
	log *slog.Logger

	mu sync.Mutex // serializes every storage mutation
}

func New(db *store.Store, bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{db: db, bus: bus, log: log}
}

// Start heals any pool left mid-mutation by a crash (dataset create/
// destroy are synchronous, so this is belt-and-braces).
func (m *Manager) Start(ctx context.Context) error {
	// Re-apply the ARC cap every boot, not just at pool creation: the cap
	// is written once into /etc/modprobe.d/zfs.conf, so a box installed
	// under an older, worse heuristic would keep it forever otherwise (the
	// v0.9 25%-of-RAM cap starved the DDT badly enough to pin a core —
	// see arcMaxBytes). Cheap, idempotent, and lets an update fix the box.
	if m.SystemReady(ctx) {
		if err := m.setARCCap(); err != nil {
			m.log.Warn("arc cap", "err", err)
		}
	}
	pools, err := m.db.ListPools(ctx)
	if err != nil {
		return err
	}
	for _, p := range pools {
		if p.State == store.PoolCreating || p.State == store.PoolRemoving {
			_ = m.db.SetPoolState(ctx, p.ID, store.PoolError,
				"interrupted by daemon restart — remove and recreate the pool")
		}
	}
	return nil
}

// SystemReady reports whether the system zpool exists.
func (m *Manager) SystemReady(ctx context.Context) bool {
	_, err := sysexec.Run(ctx, 10*time.Second, "zpool", "list", "-H", "-o", "name", Zpool)
	return err == nil
}

// SystemStatus describes the system zpool for the Storage UI.
type SystemStatus struct {
	Ready      bool     `json:"ready"`
	Zpool      string   `json:"zpool"`
	DataDevs   []string `json:"data_devs"`
	DedupDev   string   `json:"dedup_dev"`
	DedupFixed bool     `json:"dedup_fixed"` // dedupe device is remembered/permanent
	DedupRatio float64  `json:"dedup_ratio"`
	SizeBytes  int64    `json:"size_bytes"`  // whole pool (data + dedupe vdevs)
	AllocBytes int64    `json:"alloc_bytes"` // whole pool
	// Per-vdev split so the UI can show data-disk vs dedupe-SSD usage
	// separately (they hold different things — tape data vs the dedup table).
	DataSizeBytes   int64 `json:"data_size_bytes"`
	DataAllocBytes  int64 `json:"data_alloc_bytes"`
	DedupSizeBytes  int64 `json:"dedup_size_bytes"`
	DedupAllocBytes int64 `json:"dedup_alloc_bytes"`
	// Compression algorithm on the system zpool (zstd since v0.9;
	// lz4 on pools built before that — shown so support can tell).
	Compression string `json:"compression,omitempty"`
}

func (m *Manager) SystemStatus(ctx context.Context) SystemStatus {
	s := SystemStatus{Zpool: Zpool}
	// The permanent dedupe device is reported even before/after the zpool
	// exists so the setup UI can lock it (chosen once, never changed).
	if fixed := m.db.Setting(ctx, dedupDeviceKey, ""); fixed != "" {
		s.DedupFixed = true
		if dev, err := resolveDev(fixed); err == nil {
			s.DedupDev = dev
		}
	}
	if !m.SystemReady(ctx) {
		return s
	}
	s.Ready = true
	for dev, role := range m.roles(ctx) {
		switch role {
		case "pool-data":
			s.DataDevs = append(s.DataDevs, dev)
		case "dedup-device":
			s.DedupDev = dev
		}
	}
	if out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "list", "-Hp", "-o", "size,alloc", Zpool); err == nil {
		if f := strings.Fields(out); len(f) >= 2 {
			s.SizeBytes, _ = strconv.ParseInt(f[0], 10, 64)
			s.AllocBytes, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	s.DataSizeBytes, s.DataAllocBytes, s.DedupSizeBytes, s.DedupAllocBytes = m.vdevUsage(ctx)
	// Pool-wide dedup ratio (dedupratio = deduped logical / allocated across
	// every dedup=on dataset). This is the honest global figure — every
	// OpenVTL pool is a dedup=on dataset in the one zpool, so it aggregates
	// dedup across all pools/libraries. 1.00x means no savings yet (e.g. an
	// empty pool), not a collection failure.
	if out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "get", "-H", "-o", "value", "dedupratio", Zpool); err == nil {
		s.DedupRatio, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(out), "x"), 64)
	}
	if out, err := sysexec.Run(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value", "compression", Zpool); err == nil {
		s.Compression = strings.TrimSpace(out)
	}
	return s
}

// vdevUsage returns per-role size/alloc (bytes) for the system zpool's data
// vdev(s) and its dedupe vdev, parsed from `zpool list -vHp` (the pool line
// resets to the data section; a `dedup` header switches sections). Lets the
// UI split capacity between the data disk(s) and the dedupe SSD.
func (m *Manager) vdevUsage(ctx context.Context) (dataSize, dataAlloc, dedupSize, dedupAlloc int64) {
	out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "list", "-vHp", Zpool)
	if err != nil {
		return
	}
	section := "data"
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case Zpool: // the pool total line — its child vdevs are summed below
			section = "data"
			continue
		case "dedup", "special", "logs", "cache", "spare":
			section = f[0]
			continue
		}
		if len(f) < 3 || f[1] == "-" {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		alloc, _ := strconv.ParseInt(f[2], 10, 64)
		switch section {
		case "data":
			dataSize += size
			dataAlloc += alloc
		case "dedup":
			dedupSize += size
			dedupAlloc += alloc
		}
	}
	return
}

// roles annotates disks that are vdevs of the system zpool (data vs the
// SSD dedup vdev). Keyed by resolved /dev path (matches Devices()).
func (m *Manager) roles(ctx context.Context) roleMap {
	r := roleMap{}
	if out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "status", "-LP", Zpool); err == nil {
		section := "data"
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			switch f[0] {
			case Zpool:
				section = "data"
				continue
			case "dedup", "special", "logs", "cache", "spares":
				section = f[0]
				continue
			}
			if strings.HasPrefix(f[0], "/dev/") {
				role := "pool-data"
				if section == "dedup" {
					role = "dedup-device"
				}
				// zpool creates a partition on a whole disk (sdc -> sdc1); the
				// enumeration lists the parent disk, so annotate that.
				r[parentDisk(f[0])] = role
			}
		}
	}
	// Keep the permanent dedupe device annotated even with no zpool (after
	// a teardown) so it is never offered as an available data disk.
	if fixed := m.db.Setting(ctx, dedupDeviceKey, ""); fixed != "" {
		if dev, err := resolveDev(fixed); err == nil {
			r[dev] = "dedup-device"
		}
	}
	return r
}

var reNvmePart = regexp.MustCompile(`^(.+\d)p\d+$`) // nvme0n1p1 -> nvme0n1
var reSDPart = regexp.MustCompile(`^(.+\D)\d+$`)    // sdc1 -> sdc

func parentDisk(dev string) string {
	if m := reNvmePart.FindStringSubmatch(dev); m != nil {
		return m[1]
	}
	if m := reSDPart.FindStringSubmatch(dev); m != nil {
		return m[1]
	}
	return dev
}

func (m *Manager) Devices(ctx context.Context) ([]BlockDevice, error) {
	return Devices(ctx, m.roles(ctx))
}

// Rescan asks every SCSI host to re-probe its bus so hot-added vHDDs
// appear without a reboot, then settles udev so by-id links exist.
func (m *Manager) Rescan(ctx context.Context) (int, error) {
	hosts, err := filepath.Glob("/sys/class/scsi_host/host*/scan")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range hosts {
		if err := os.WriteFile(h, []byte("- - -\n"), 0o200); err != nil {
			m.log.Warn("scsi host rescan", "host", h, "err", err)
			continue
		}
		n++
	}
	_, _ = sysexec.Run(ctx, 15*time.Second, "udevadm", "settle", "--timeout=5")
	m.log.Info("scsi bus rescan", "hosts", n)
	return n, nil
}

// SetupSystemPool creates the system zpool from data disk(s) on HDD plus
// one SSD dedup vdev (the one-time storage foundation). confirm must be
// "create" typed back — it erases the selected disks.
func (m *Manager) SetupSystemPool(ctx context.Context, dataDevs []string, dedupDev, confirm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if confirm != "create" {
		return fmt.Errorf(`type "create" to confirm — this erases the selected disks`)
	}
	if m.SystemReady(ctx) {
		return fmt.Errorf("system storage is already set up")
	}
	if len(dataDevs) == 0 {
		return fmt.Errorf("select at least one data disk")
	}
	if dedupDev == "" {
		return fmt.Errorf("select an SSD to hold the dedupe metadata")
	}
	// The dedupe device is permanent. If one was chosen on an earlier setup
	// (remembered across a pool teardown), it cannot be swapped — ZFS can't
	// relocate a dedup vdev.
	if fixed := m.db.Setting(ctx, dedupDeviceKey, ""); fixed != "" && byIDPath(dedupDev) != fixed {
		return fmt.Errorf("the dedupe device is fixed for this system and can't be changed")
	}
	for _, d := range dataDevs {
		if d == dedupDev {
			return fmt.Errorf("a disk cannot be both data and dedupe metadata")
		}
	}
	for _, d := range append(append([]string{}, dataDevs...), dedupDev) {
		if err := VerifyBare(ctx, d); err != nil {
			return err
		}
	}
	if err := m.setARCCap(); err != nil {
		m.log.Warn("arc cap", "err", err)
	}
	// zstd over lz4 (v0.9): measured on a real IBM i save stream —
	// 1.46× vs 1.26× at 1M (~16% more capacity) AND faster wall-time on
	// a disk-bound box (less physical data written). zstd-9 measured
	// identical to zstd-3 at +43% time, so the default level stands.
	args := []string{"create", "-f", "-o", "ashift=12",
		"-O", "compression=zstd", "-O", "atime=off", "-O", "xattr=sa",
		"-m", "none", Zpool}
	for _, d := range dataDevs {
		args = append(args, byIDPath(d))
	}
	if err := run(ctx, "zpool", args...); err != nil {
		return err
	}
	if err := run(ctx, "zpool", "add", "-f", Zpool, "dedup", byIDPath(dedupDev)); err != nil {
		// Roll back the just-created data-only zpool so a failed setup
		// leaves the disks clean and re-runnable (no half-built pool that
		// SystemReady would call "already set up").
		_ = run(ctx, "zpool", "destroy", Zpool)
		return err
	}
	// Fast Dedup (ZFS 2.3): bound the DDT by the dedupe SSD's capacity —
	// when it fills, new writes simply stop deduping (graceful) instead
	// of thrashing. Warn-only: older ZFS lacks the property.
	if err := run(ctx, "zpool", "set", "dedup_table_quota=auto", Zpool); err != nil {
		m.log.Warn("dedup_table_quota not set (older ZFS?)", "err", err)
	}
	// Remember the dedupe device as this system's permanent metadata home.
	if err := m.db.SetSetting(ctx, dedupDeviceKey, byIDPath(dedupDev)); err != nil {
		m.log.Warn("persist dedupe device", "err", err)
	}
	m.log.Info("system zpool created", "pool", Zpool, "data", dataDevs, "dedup", dedupDev)
	m.bus.Publish("storage_setup", Zpool, map[string]any{"data": dataDevs, "dedup": dedupDev})
	return nil
}

// CreatePool creates a dataset in the system zpool (dedup=on). Fast and
// synchronous — no disk selection, no cache slice; all pools share the
// system zpool's disks and its single dedup vdev.
func (m *Manager) CreatePool(ctx context.Context, name string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !poolNameRe.MatchString(name) {
		return 0, fmt.Errorf("pool name must be lowercase letters, digits or underscore (2-24 chars)")
	}
	if !m.SystemReady(ctx) {
		return 0, fmt.Errorf("set up system storage first — pick data disks and an SSD for dedupe metadata")
	}
	pools, err := m.db.ListPools(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range pools {
		if p.Name == name {
			return 0, fmt.Errorf("pool %s already exists", name)
		}
	}
	dataset := Zpool + "/" + name
	mount := PoolsRoot + "/" + name
	recordSize := recordSizeFromRAM()
	if err := run(ctx, "zfs", "create", "-o", "dedup=on",
		"-o", "recordsize="+recordSize, "-o", "mountpoint=legacy", dataset); err != nil {
		return 0, err
	}
	m.log.Info("pool recordsize scaled from RAM", "pool", name, "recordsize", recordSize,
		"mem_total_gib", totalRAMBytes()>>30)
	if err := m.mountDataset(ctx, dataset, mount); err != nil {
		_ = run(ctx, "zfs", "destroy", dataset)
		return 0, err
	}
	id, err := m.db.CreatePool(ctx, store.Pool{
		Name: name, VG: Zpool, DataLV: name, Mountpoint: mount,
		DataDev: "", State: store.PoolActive,
	})
	if err != nil {
		return 0, err
	}
	m.log.Info("pool dataset created", "pool", name, "dataset", dataset, "mount", mount)
	m.bus.Publish("pool_created", name, map[string]any{"mountpoint": mount})
	return id, nil
}

// RemovePool destroys a pool's dataset, leaving the system zpool and its
// disks intact. Refused while a library is still paired (1:1 model). The
// pool name must be typed back. Synchronous and idempotent.
func (m *Manager) RemovePool(ctx context.Context, poolID int64, confirm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, err := m.db.GetPool(ctx, poolID)
	if err != nil {
		return err
	}
	if confirm != p.Name {
		return fmt.Errorf("confirmation mismatch: type the pool name %q back", p.Name)
	}
	libs, err := m.db.ListLibraries(ctx)
	if err != nil {
		return err
	}
	for _, l := range libs {
		if l.HomePool == poolID {
			return fmt.Errorf("library %q (%s) is still paired to this pool — delete the library first", l.Name, l.Serial)
		}
	}
	dataset := p.VG + "/" + p.DataLV
	if mounted, err := isMounted(p.Mountpoint); err != nil {
		return err
	} else if mounted {
		if err := run(ctx, "umount", p.Mountpoint); err != nil {
			return err
		}
	}
	if err := m.removeFstabLine(ctx, p.Mountpoint); err != nil {
		return err
	}
	if err := run(ctx, "zfs", "destroy", "-r", dataset); err != nil &&
		!strings.Contains(err.Error(), "does not exist") {
		return err
	}
	_ = os.Remove(p.Mountpoint)
	if err := m.db.DeletePool(ctx, poolID); err != nil {
		return err
	}
	m.log.Info("pool removed", "pool", p.Name, "dataset", dataset)
	m.bus.Publish("pool_removed", p.Name, map[string]any{"dataset": dataset})
	// Removing a pool no longer tears the system zpool down — freeing the
	// data disk(s) is a deliberate, separate step (TeardownSystem), so
	// removing the last pool leaves storage set up and ready to add another.
	return nil
}

// TeardownSystem destroys the system zpool and frees the data disk(s) — the
// inverse of SetupSystemPool, and a deliberate step separate from pool
// removal. Valid only once every pool is gone. The dedupe SSD stays
// remembered as this system's permanent metadata device. confirm = "teardown".
func (m *Manager) TeardownSystem(ctx context.Context, confirm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if confirm != "teardown" {
		return fmt.Errorf(`type "teardown" to confirm — this destroys the system storage and erases its disks`)
	}
	if !m.SystemReady(ctx) {
		return fmt.Errorf("system storage is not set up")
	}
	pools, err := m.db.ListPools(ctx)
	if err != nil {
		return err
	}
	if len(pools) > 0 {
		return fmt.Errorf("remove all %d pool(s) first — teardown frees the disks", len(pools))
	}
	return m.teardownSystem(ctx)
}

// GrowSystem expands the system zpool's vdevs onto disks enlarged underneath
// it (e.g. the hypervisor grew a vHDD). It covers BOTH the data disk(s) and
// the dedupe SSD — growing the data disk lets the pool hold more tape data,
// and growing the dedupe disk keeps the dedup table (which scales with unique
// data) on fast storage as the pool fills. For each vdev it re-reads the disk
// size, then runs `zpool online -e` (by-id) so ZFS grows its partition and
// the pool's free space; every pool is a dataset sharing it, so nothing
// per-pool resizes. Online, non-destructive and idempotent (a per-vdev no-op
// when there's nothing new to claim). Returns pool size before/after.
func (m *Manager) GrowSystem(ctx context.Context) (before, after int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.SystemReady(ctx) {
		return 0, 0, fmt.Errorf("system storage is not set up")
	}
	var devs []string
	for dev, role := range m.roles(ctx) {
		if role == "pool-data" || role == "dedup-device" {
			devs = append(devs, dev)
		}
	}
	if len(devs) == 0 {
		return 0, 0, fmt.Errorf("no system-pool disks found")
	}
	before = m.zpoolSize(ctx)
	for _, dev := range devs {
		// Best-effort: make the kernel re-read the (grown) disk size before
		// asking ZFS to expand onto it. zpool needs the by-id vdev name.
		if name := filepath.Base(dev); name != "" {
			_ = os.WriteFile("/sys/block/"+name+"/device/rescan", []byte("1\n"), 0o200)
		}
		if err := run(ctx, "zpool", "online", "-e", Zpool, byIDPath(dev)); err != nil {
			return before, m.zpoolSize(ctx), fmt.Errorf("expand %s: %w", dev, err)
		}
	}
	_, _ = sysexec.Run(ctx, 15*time.Second, "udevadm", "settle", "--timeout=5")
	after = m.zpoolSize(ctx)
	m.log.Info("system storage grown", "before_bytes", before, "after_bytes", after, "disks", devs)
	m.bus.Publish("storage_grown", Zpool, map[string]any{"before": before, "after": after})
	return before, after, nil
}

// zpoolSize returns the system zpool's total size in bytes (0 on error).
func (m *Manager) zpoolSize(ctx context.Context) int64 {
	if out, err := sysexec.Run(ctx, 10*time.Second, "zpool", "list", "-Hp", "-o", "size", Zpool); err == nil {
		n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		return n
	}
	return 0
}

// teardownSystem destroys the now-empty system zpool and releases the
// data disk(s) to the available list — the inverse of SetupSystemPool,
// run from TeardownSystem once every pool is gone. The dedupe SSD's identity stays
// remembered (dedupDeviceKey) as the permanent metadata device; every
// disk is wiped clean so the data disk re-lists as available and the
// dedupe disk can be re-added by the next setup.
func (m *Manager) teardownSystem(ctx context.Context) error {
	var dataDevs []string
	dedup := ""
	for dev, role := range m.roles(ctx) {
		switch role {
		case "pool-data":
			dataDevs = append(dataDevs, dev)
		case "dedup-device":
			dedup = dev
		}
	}
	if err := run(ctx, "zpool", "destroy", Zpool); err != nil {
		return fmt.Errorf("destroy system zpool: %w", err)
	}
	// Clear signatures so lsblk re-lists the disks. The data disk becomes
	// available; the dedupe disk stays reserved via roles()' dedupDeviceKey
	// overlay even though it too is now bare.
	for _, d := range append(append([]string{}, dataDevs...), dedup) {
		if d == "" {
			continue
		}
		if err := run(ctx, "wipefs", "-a", d); err != nil {
			m.log.Warn("wipe device on teardown", "dev", d, "err", err)
		}
	}
	_, _ = sysexec.Run(ctx, 15*time.Second, "udevadm", "settle", "--timeout=5")
	m.log.Info("system storage torn down (explicit teardown)",
		"data_released", dataDevs, "dedup_remembered", dedup)
	m.bus.Publish("storage_torndown", Zpool, map[string]any{"data": dataDevs, "dedup": dedup})
	return nil
}

// --- helpers ---

func run(ctx context.Context, name string, args ...string) error {
	return runFor(ctx, 120*time.Second, name, args...)
}

func runFor(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	if _, err := sysexec.Run(ctx, timeout, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// byIDPath resolves a /dev/sdX to a stable by-id path for zpool (device
// naming survives letter shuffles across boots).
func byIDPath(dev string) string {
	if id := findByID(dev); id != "" {
		return id
	}
	return dev
}

// setARCCap caps the ARC now and persists it for reboots.
func (m *Manager) setARCCap() error {
	arc := arcMaxBytes()
	m.log.Info("arc cap scaled from RAM", "arc_max_mib", arc>>20, "mem_total_gib", totalRAMBytes()>>30)
	if err := os.WriteFile("/etc/modprobe.d/zfs.conf",
		[]byte(fmt.Sprintf("options zfs zfs_arc_max=%d\n", arc)), 0o644); err != nil {
		return err
	}
	return os.WriteFile("/sys/module/zfs/parameters/zfs_arc_max",
		[]byte(strconv.FormatInt(arc, 10)), 0o644)
}

func (m *Manager) mountDataset(ctx context.Context, dataset, mount string) error {
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return err
	}
	if err := addFstab(dataset, mount); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "mount", mount)
}

// addFstab writes a legacy ZFS mount line so mhVTL's RequiresMountsFor
// drop-ins bind to a real .mount unit (ordered after zfs-import.target).
func addFstab(dataset, mount string) error {
	b, _ := os.ReadFile("/etc/fstab")
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, " "+mount+" ") {
			lines = append(lines, l)
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	lines = append(lines,
		fmt.Sprintf("%s %s zfs defaults,nofail,x-systemd.requires=zfs-import.target 0 0", dataset, mount), "")
	return os.WriteFile("/etc/fstab", []byte(strings.Join(lines, "\n")), 0o644)
}

func (m *Manager) removeFstabLine(ctx context.Context, mount string) error {
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var lines []string
	changed := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, " "+mount+" ") {
			changed = true
			continue
		}
		lines = append(lines, l)
	}
	if !changed {
		return nil
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	lines = append(lines, "")
	if err := os.WriteFile("/etc/fstab", []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	return run(ctx, "systemctl", "daemon-reload")
}

func isMounted(path string) (bool, error) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == path {
			return true, nil
		}
	}
	return false, nil
}
