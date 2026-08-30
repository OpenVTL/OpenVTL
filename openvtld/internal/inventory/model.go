package inventory

import "time"

// Snapshot is the complete observable state of the VTL, maintained by
// the engine and served read-only to the API. Copies are returned to
// callers; the engine owns the single mutable instance. Since v0.6 it
// is list-shaped: N libraries, N pools.
type Snapshot struct {
	UpdatedAt time.Time         `json:"updated_at"`
	Libraries []LibrarySnapshot `json:"libraries"`
	Pools     []PoolStats       `json:"pools"`
	FC        FCState           `json:"fc"`
}

// LibrarySnapshot is one library's live state. A library declared in
// device.conf but not yet served by mhVTL (pending_restart from the
// create wizard) appears with Live=false and empty Drives/Slots.
type LibrarySnapshot struct {
	Library Library `json:"library"`
	Drives  []Drive `json:"drives"`
	Slots   []Slot  `json:"slots"`
	Carts   []Cart  `json:"cartridges"`
}

type Library struct {
	ID        int    `json:"id"` // mhVTL library id (10, 20, …)
	Product   string `json:"product"`
	Serial    string `json:"serial"`
	HomeDir   string `json:"home_dir"`
	NumSlots  int    `json:"num_slots"`
	NumIE     int    `json:"num_ie"`
	NumDrives int    `json:"num_drives"`
	ChangerSG string `json:"changer_sg"`
	Live      bool   `json:"live"` // changer sg node found + polling
}

type Drive struct {
	Index      int    `json:"index"`    // 0-based index within its library
	Library    int    `json:"library"`  // owning library id
	QueueID    int    `json:"queue_id"` // mhVTL daemon queue (vtltape@NN)
	Serial     string `json:"serial"`
	Product    string `json:"product"`
	SG         string `json:"sg"`
	ST         string `json:"st"`
	Loaded     string `json:"loaded"`      // cart label or ""
	SourceSlot int    `json:"source_slot"` // home slot of loaded cart (0 unknown)
	Activity   string `json:"activity"`    // idle | reading | writing | mounting
	// Rolling activity counters (this boot)
	BlocksWritten int64     `json:"blocks_written"`
	BlocksRead    int64     `json:"blocks_read"`
	LastActive    time.Time `json:"last_active"`
}

type Slot struct {
	Kind  string `json:"kind"` // storage | ie
	Num   int    `json:"num"`  // 1-based within kind (mtx numbering)
	Label string `json:"label"`
}

type Cart struct {
	Label     string    `json:"label"`
	Library   int       `json:"library"` // owning library id
	SizeBytes int64     `json:"size_bytes"`
	PhysBytes int64     `json:"phys_bytes"` // allocated on disk (post-compression)
	Modified  time.Time `json:"modified"`
	Location  string    `json:"location"` // slot:N | ie:N | drive:N | missing
}

type PoolStats struct {
	Name         string  `json:"name"` // pool name (legacy pre-v0.6: the -vdo-lv flag value)
	Mountpoint   string  `json:"mountpoint"`
	FSTotalBytes int64   `json:"fs_total_bytes"`
	FSUsedBytes  int64   `json:"fs_used_bytes"`
	VDOPhysBytes int64   `json:"vdo_phys_bytes"`
	VDOUsedBytes int64   `json:"vdo_used_bytes"`
	VDOSavingPct int     `json:"vdo_saving_pct"`
	CacheUsedPct float64 `json:"cache_used_pct"`
	// ZFS storage plane (v0.7): dedupe is pool-wide (the shared dedup
	// vdev), compression is per-dataset; logical is pre-dedup/compress.
	DedupRatio    float64 `json:"dedup_ratio"`
	CompressRatio float64 `json:"compress_ratio"`
	LogicalBytes  int64   `json:"logical_bytes"`
	RecordBytes   int64   `json:"record_bytes"` // dedupe granularity (RAM-scaled at creation, v0.9)
	// Real-disk figures (v0.9 fix): dataset `used` (VDOUsedBytes) is
	// post-compression only — global dedup savings appear at zpool
	// level, so it can exceed the pool size and must never be shown as
	// "physical". PhysEst ≈ used ÷ global dedupratio.
	PhysEstBytes    int64     `json:"phys_est_bytes"`
	ZpoolSizeBytes  int64     `json:"zpool_size_bytes"`  // whole system zpool (shared)
	ZpoolAllocBytes int64     `json:"zpool_alloc_bytes"` // real allocated incl. dedup
	CollectedAt     time.Time `json:"collected_at"`
}

type FCState struct {
	// TargetWWN is empty by design: every target-capable port serves the
	// same LUN table (FC is symmetric — there is no single target WWN to
	// name). The field survives for API-shape stability only; per-port
	// WWNs live in the Access view / PortsView.
	TargetWWN  string    `json:"target_wwn"`
	Verified   bool      `json:"verified"`
	NoHBA      bool      `json:"no_hba"` // no FC hardware: idle is the healthy state, not a fault
	Detail     string    `json:"detail"`
	VerifiedAt time.Time `json:"verified_at"`
}

// AllCarts flattens every library's carts (each Cart carries its
// library id).
func (s Snapshot) AllCarts() []Cart {
	var out []Cart
	for _, l := range s.Libraries {
		out = append(out, l.Carts...)
	}
	return out
}

// LibraryByID returns the snapshot of one library.
func (s Snapshot) LibraryByID(id int) (LibrarySnapshot, bool) {
	for _, l := range s.Libraries {
		if l.Library.ID == id {
			return l, true
		}
	}
	return LibrarySnapshot{}, false
}

// FreeStorageSlots counts the empty storage slots (not I/E) a library has —
// the number of carts that can still be minted/adopted into it. A cart
// loaded in a drive leaves its home slot reading empty; that edge is left
// to the per-cart free-slot search (it skips reserved home slots), so this
// is a small over-estimate only while carts are loaded.
func (l LibrarySnapshot) FreeStorageSlots() int {
	n := 0
	for _, s := range l.Slots {
		if s.Kind == "storage" && s.Label == "" {
			n++
		}
	}
	return n
}

// FindCart locates a cart and the library holding it.
func (s Snapshot) FindCart(label string) (Cart, LibrarySnapshot, bool) {
	for _, l := range s.Libraries {
		for _, c := range l.Carts {
			if c.Label == label {
				return c, l, true
			}
		}
	}
	return Cart{}, LibrarySnapshot{}, false
}
