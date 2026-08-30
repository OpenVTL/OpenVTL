package export

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/s3"
	"github.com/openvtl/openvtld/internal/store"
)

// runImport drives: queued -> fetching -> verifying -> unpacking ->
// done. Chunk hashes are checked while streaming, file hashes while
// extracting; unpack is a rename swap so a half-written import can
// never masquerade as a cart.
func (r *Runner) runImport(ctx context.Context, j *store.Job) error {
	if j.RemoteID == nil {
		return fmt.Errorf("import job has no remote")
	}
	if j.Generation == "" {
		return fmt.Errorf("import job has no generation")
	}
	remote, err := r.db.GetRemote(ctx, *j.RemoteID)
	if err != nil {
		return fmt.Errorf("remote %d: %w", *j.RemoteID, err)
	}
	cl, err := s3.New(remote)
	if err != nil {
		return err
	}

	// Manifest: catalog cache first, bucket direct as fallback (a
	// fresh daemon can import its OWN cart before anyone clicks "rebuild").
	var m *catalog.Manifest
	if raw, err := r.db.GetManifestJSON(ctx, *j.RemoteID, j.SystemName, j.CartLabel, j.Generation); err == nil {
		if m, err = catalog.Decode([]byte(raw)); err != nil {
			return fmt.Errorf("cached manifest: %w", err)
		}
	} else if j.SystemName != "" {
		// Foreign cart (another system's export): the object key needs
		// that system + its library serial, which live only in the cached
		// manifest — the operator must rebuild the catalog first.
		return fmt.Errorf("manifest for %s/%s generation %s is not in the catalog cache — rebuild from the bucket first", j.SystemName, j.CartLabel, j.Generation)
	} else {
		// Same-system, not cached (fresh daemon before a rebuild): build
		// the key from this system + the cart's library serial. The chunk
		// keys inside are self-contained.
		system, _ := r.systemIdentity(ctx)
		library := r.libSerialFor(j.CartLabel)
		raw, err := cl.GetBytes(ctx, cl.Key(catalog.ManifestKeyParts(system, library, j.CartLabel, j.Generation)...))
		if err != nil {
			return fmt.Errorf("manifest fetch: %w", err)
		}
		if m, err = catalog.Decode(raw); err != nil {
			return err
		}
		r.db.UpsertCatalogEntry(ctx, store.CatalogEntry{
			RemoteID: *j.RemoteID, SystemName: m.Source.SystemName, LibrarySerial: m.Source.LibrarySerial,
			CartLabel: m.Label, Generation: m.Generation,
			ManifestJSON: string(raw), LogicalBytes: m.Totals.LogicalBytes,
			StoredBytes: m.Totals.StoredBytes, ChunkCount: m.Totals.ChunkCount,
			ExportedAt: m.ExportedAt,
		})
	}
	if m.Label != j.CartLabel {
		return fmt.Errorf("manifest is for %s, job is for %s", m.Label, j.CartLabel)
	}

	// Destination: a foreign import lands in the chosen target library's
	// home dir (the label is new here); a same-system re-import uses the
	// cart's own library dir.
	var mediaRoot string
	if j.TargetLibrary != nil {
		mediaRoot, err = r.inv.LibraryHome(ctx, int(*j.TargetLibrary))
		if err != nil {
			return fmt.Errorf("target library %d: %w", *j.TargetLibrary, err)
		}
	} else {
		mediaRoot = r.mediaDirFor(j.CartLabel)
	}
	dir := filepath.Join(mediaRoot, j.CartLabel)
	dirExists := false
	if _, err := os.Stat(dir); err == nil {
		dirExists = true
		if loc, ok := r.cartLocation(j.CartLabel); ok && strings.HasPrefix(loc, "drive:") {
			return fmt.Errorf("cart %s is loaded in %s — unload from the host first", j.CartLabel, loc)
		}
		// Same-system re-import must not silently clobber resident data
		// (evict first). A foreign import into a chosen library is an
		// explicit operator action, and a leftover dir there is a prior
		// failed attempt — safe to overwrite.
		if j.TargetLibrary == nil {
			if _, err := os.Stat(filepath.Join(dir, markerFile)); err != nil {
				// No eviction marker: resident data. Only allow import
				// over media that has no data files at all (label-only).
				if files, err := listCartFiles(dir); err == nil {
					for _, fi := range files {
						if strings.HasPrefix(fi.Name(), "data.") && fi.Size() > 0 {
							return fmt.Errorf("cart %s has resident data — evict it before importing generation %s", j.CartLabel, j.Generation)
						}
					}
				}
			}
		}
	}

	if err := r.db.SetJobTotals(ctx, j.ID, m.Totals.LogicalBytes, m.Totals.ChunkCount); err != nil {
		return err
	}
	if err := r.transition(ctx, j, "fetching",
		fmt.Sprintf("generation %s: %d chunks, %s stored", m.Generation, m.Totals.ChunkCount, human(m.Totals.StoredBytes))); err != nil {
		return err
	}

	// Extract into a staging dir on the pool filesystem so the final
	// swap is a rename, not a copy.
	stage := filepath.Join(mediaRoot, ".openvtl-import-"+j.CartLabel)
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	cr := &chunkStream{chunks: m.Chunks, open: func(key string) (io.ReadCloser, error) {
		return cl.Get(ctx, key)
	}}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return err
	}
	defer dec.Close()
	cr.dec = dec

	type extracted struct {
		size   int64
		sha256 string
	}
	got := map[string]extracted{}
	var doneBytes int64
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar stream: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || strings.Contains(hdr.Name, "/") {
			return fmt.Errorf("unexpected tar entry %q", hdr.Name)
		}
		dst := filepath.Join(stage, hdr.Name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), tr)
		cerr := f.Close()
		if err != nil {
			return fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
		if cerr != nil {
			return cerr
		}
		os.Chown(dst, hdr.Uid, hdr.Gid)
		os.Chtimes(dst, hdr.ModTime, hdr.ModTime)
		got[hdr.Name] = extracted{size: n, sha256: hex.EncodeToString(h.Sum(nil))}
		doneBytes += n
		r.progress(ctx, j.ID, doneBytes, cr.done)
	}
	if err := cr.finish(); err != nil {
		return err
	}

	if err := r.transition(ctx, j, "verifying",
		fmt.Sprintf("%d files extracted; comparing against manifest", len(got))); err != nil {
		return err
	}
	if len(got) != len(m.CartFiles) {
		return fmt.Errorf("extracted %d files, manifest lists %d", len(got), len(m.CartFiles))
	}
	for _, mf := range m.CartFiles {
		g, ok := got[mf.Name]
		if !ok {
			return fmt.Errorf("manifest file %s missing from archive", mf.Name)
		}
		if g.size != mf.Size {
			return fmt.Errorf("%s: size %d != manifest %d", mf.Name, g.size, mf.Size)
		}
		if g.sha256 != mf.SHA256 {
			return fmt.Errorf("%s: sha256 mismatch — corrupt transfer", mf.Name)
		}
	}

	if err := r.transition(ctx, j, "unpacking", "swapping verified media into place"); err != nil {
		return err
	}
	if dirExists {
		old := dir + ".pre-import"
		os.RemoveAll(old)
		if err := os.Rename(dir, old); err != nil {
			return fmt.Errorf("set aside old media: %w", err)
		}
		if err := os.Rename(stage, dir); err != nil {
			os.Rename(old, dir) // restore; stage cleanup via defer
			return fmt.Errorf("swap in: %w", err)
		}
		os.RemoveAll(old)
	} else {
		if err := os.Rename(stage, dir); err != nil {
			return fmt.Errorf("place media: %w", err)
		}
	}
	// Match mhVTL's expected dir ownership/mode (parent is vtl:vtl,
	// cart dirs on this system are root-owned 0750).
	os.Chmod(dir, 0o750)

	// Foreign import: the label is new to the target library — take the
	// media through the MAP into a free slot so the host sees it (Phase A
	// cross-instance import). Same-system re-imports already own a slot.
	slotted := ""
	if j.TargetLibrary != nil {
		if err := r.transition(ctx, j, "slotting", "assigning a storage slot in the target library"); err != nil {
			return err
		}
		slot, err := r.inv.AdoptCart(ctx, int(*j.TargetLibrary), j.CartLabel)
		if err != nil {
			return fmt.Errorf("slot assignment: %w", err)
		}
		var sz int64
		for name, e := range got {
			if strings.HasPrefix(name, "data.") {
				sz += e.size
			}
		}
		if err := r.db.UpsertCartridge(ctx, j.CartLabel, sz, "", nil, int(*j.TargetLibrary)); err != nil {
			r.log.Warn("cartridge row upsert after import", "label", j.CartLabel, "err", err)
		}
		slotted = fmt.Sprintf(" → slot %d", slot)
	}

	if err := r.db.SetCartLocalState(ctx, j.CartLabel, "resident", j.Generation); err != nil {
		return err
	}

	detail := fmt.Sprintf("generation %s restored: %d files, %s%s", m.Generation, len(got), human(doneBytes), slotted)
	if j.TargetLibrary == nil {
		if _, ok := r.cartLocation(j.CartLabel); !ok {
			detail += " (label unknown to library — slot assignment pending)"
		}
	}
	if err := r.transition(ctx, j, "done", detail); err != nil {
		return err
	}
	r.db.LogEvent(ctx, time.Now(), "import_done", j.CartLabel, detail)
	return nil
}

// chunkStream concatenates the decompressed chunk objects into the one
// logical tar stream, verifying each chunk's SHA-256 as it drains. The
// opener indirection keeps it testable without S3.
type chunkStream struct {
	chunks []catalog.Chunk
	open   func(key string) (io.ReadCloser, error)
	dec    *zstd.Decoder

	cur     io.ReadCloser // raw chunk object
	hash    hash.Hash
	tee     io.Reader
	next    int
	done    int
	inFrame bool
}

func (c *chunkStream) advance() error {
	if c.next >= len(c.chunks) {
		return io.EOF
	}
	ch := c.chunks[c.next]
	obj, err := c.open(ch.Key)
	if err != nil {
		return err
	}
	c.cur = obj
	h := sha256.New()
	c.hash = h
	c.tee = io.TeeReader(obj, h)
	if err := c.dec.Reset(c.tee); err != nil {
		obj.Close()
		return err
	}
	c.inFrame = true
	c.next++
	return nil
}

// endChunk drains, hash-verifies, and closes the current object.
func (c *chunkStream) endChunk() error {
	if _, err := io.Copy(io.Discard, c.tee); err != nil {
		c.cur.Close()
		return err
	}
	c.cur.Close()
	ch := c.chunks[c.next-1]
	sum := hex.EncodeToString(c.hash.Sum(nil))
	if sum != ch.SHA256 {
		return fmt.Errorf("chunk %d: sha256 mismatch (transfer corrupt)", ch.Idx)
	}
	c.inFrame = false
	c.done++
	return nil
}

func (c *chunkStream) Read(p []byte) (int, error) {
	for {
		if !c.inFrame {
			if err := c.advance(); err != nil {
				return 0, err
			}
		}
		n, err := c.dec.Read(p)
		if err == io.EOF {
			if eerr := c.endChunk(); eerr != nil {
				return n, eerr
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// finish asserts the stream fully consumed every chunk.
func (c *chunkStream) finish() error {
	if c.inFrame {
		if err := c.endChunk(); err != nil {
			return err
		}
	}
	if c.next != len(c.chunks) {
		return fmt.Errorf("tar ended after %d of %d chunks — truncated archive", c.next, len(c.chunks))
	}
	return nil
}

// readEvictionMarker loads the stub marker if present.
func readEvictionMarker(dir string) (*EvictionMarker, error) {
	b, err := os.ReadFile(filepath.Join(dir, markerFile))
	if err != nil {
		return nil, err
	}
	var m EvictionMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

type EvictionMarker struct {
	Label      string `json:"label"`
	Generation string `json:"generation"`
	RemoteID   int64  `json:"remote_id"`
	EvictedAt  string `json:"evicted_at"`
}
