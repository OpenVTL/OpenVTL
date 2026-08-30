package export

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/s3"
	"github.com/openvtl/openvtld/internal/store"
)

// runEvict drives: queued -> evicting -> done. Local data is only
// deleted after the backing manifest is re-verified in the bucket —
// eviction is the pool pressure valve, never a data-loss path.
//
// The stub keeps mam + mhvtl_data (identity) plus a marker file; the
// data.*/indx.*/meta.* content goes. Mounting a stub raises a media
// error on the host (IBM i: "Media error on volume *N", observed
// 2026-07-03) — loud and unoverwritable, which is what we want; the
// UI warns before anyone mounts one.
func (r *Runner) runEvict(ctx context.Context, j *store.Job) error {
	if j.RemoteID == nil {
		return fmt.Errorf("evict job has no remote")
	}
	remote, err := r.db.GetRemote(ctx, *j.RemoteID)
	if err != nil {
		return fmt.Errorf("remote %d: %w", *j.RemoteID, err)
	}
	cl, err := s3.New(remote)
	if err != nil {
		return err
	}

	if err := r.transition(ctx, j, "evicting", "re-verifying export in bucket before deleting local data"); err != nil {
		return err
	}
	gen := j.Generation
	if gen == "" {
		metas, err := r.db.CartMetas(ctx)
		if err != nil {
			return err
		}
		m, ok := metas[j.CartLabel]
		if !ok || m.LastExportGen == "" {
			return fmt.Errorf("cart %s has no completed export — export before evicting", j.CartLabel)
		}
		gen = m.LastExportGen
		if err := r.db.SetJobGeneration(ctx, j.ID, gen); err != nil {
			return err
		}
		j.Generation = gen
	}

	// Safety gate: the manifest must exist and parse, and its chunk
	// objects must all be present, before a single local byte goes. The
	// cart is local, so its key is this system + the cart's library serial.
	system, _ := r.systemIdentity(ctx)
	library := r.libSerialFor(j.CartLabel)
	raw, err := cl.GetBytes(ctx, cl.Key(catalog.ManifestKeyParts(system, library, j.CartLabel, gen)...))
	if err != nil {
		return fmt.Errorf("backing manifest not retrievable — refusing to evict: %w", err)
	}
	m, err := catalog.Decode(raw)
	if err != nil {
		return fmt.Errorf("backing manifest invalid — refusing to evict: %w", err)
	}
	if err := verifyChunksPresent(ctx, cl, m); err != nil {
		return fmt.Errorf("refusing to evict: %w", err)
	}

	if err := r.quiesceCheck(j.CartLabel); err != nil {
		return err
	}
	dir := filepath.Join(r.mediaDirFor(j.CartLabel), j.CartLabel)
	if _, err := os.Stat(filepath.Join(dir, markerFile)); err == nil {
		return fmt.Errorf("cart %s is already evicted", j.CartLabel)
	}

	// Consistency: only evict the exact bytes the manifest describes.
	files, err := listCartFiles(dir)
	if err != nil {
		return err
	}
	sizes := map[string]int64{}
	for _, fi := range files {
		sizes[fi.Name()] = fi.Size()
	}
	for _, mf := range m.CartFiles {
		if sizes[mf.Name] != mf.Size {
			return fmt.Errorf("cart %s changed since export %s (%s: %d != %d) — re-export before evicting",
				j.CartLabel, gen, mf.Name, sizes[mf.Name], mf.Size)
		}
	}

	marker, _ := json.Marshal(EvictionMarker{
		Label: j.CartLabel, Generation: gen, RemoteID: *j.RemoteID,
		EvictedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(dir, markerFile), marker, 0o640); err != nil {
		return err
	}
	var freed int64
	for _, fi := range files {
		n := fi.Name()
		if strings.HasPrefix(n, "data.") || strings.HasPrefix(n, "indx.") || strings.HasPrefix(n, "meta.") {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return fmt.Errorf("evict %s: %w", n, err)
			}
			freed += fi.Size()
		}
	}
	if err := r.db.SetCartLocalState(ctx, j.CartLabel, "evicted", gen); err != nil {
		return err
	}

	detail := fmt.Sprintf("stub left (mam + marker); %s freed; import generation %s to restore", human(freed), gen)
	if err := r.transition(ctx, j, "done", detail); err != nil {
		return err
	}
	r.db.LogEvent(ctx, time.Now(), "evict_done", j.CartLabel, detail)
	return nil
}

func verifyChunksPresent(ctx context.Context, cl *s3.Client, m *catalog.Manifest) error {
	for _, c := range m.Chunks {
		oi, err := cl.Stat(ctx, c.Key)
		if err != nil {
			return fmt.Errorf("chunk %d missing in bucket: %w", c.Idx, err)
		}
		if oi.Size != c.StoredBytes {
			return fmt.Errorf("chunk %d wrong size in bucket (%d != %d)", c.Idx, oi.Size, c.StoredBytes)
		}
	}
	return nil
}
