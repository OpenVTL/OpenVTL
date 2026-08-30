package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/s3"
	"github.com/openvtl/openvtld/internal/store"
)

// ensureSystemMarker writes <system>/.openvtl-system.json once per bucket
// (idempotent — preserves the original created_at) so any instance
// rebuilding the bucket can enumerate systems and flag a friendly name
// already owned by a different instance UUID.
func (r *Runner) ensureSystemMarker(ctx context.Context, cl *s3.Client, system, uuid string) error {
	key := cl.Key(catalog.SystemMarkerKeyParts(system)...)
	if _, err := cl.Stat(ctx, key); err == nil {
		return nil // already present
	}
	body, _ := json.Marshal(catalog.SystemMarker{
		SystemName: system, InstanceUUID: uuid, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return cl.PutFile(ctx, key, bytes.NewReader(body), int64(len(body)), "application/json")
}

// ensureTopology writes/refreshes the library's descriptor at
// <system>/<serial>/topology.json so a fresh box can recreate the library
// during one-click recovery. Best-effort: an export never fails on it.
func (r *Runner) ensureTopology(ctx context.Context, cl *s3.Client, system, uuid, serial, label string) {
	_, lib, ok := r.inv.Snapshot().FindCart(label)
	if !ok {
		return
	}
	row, err := r.db.GetLibrary(ctx, lib.Library.ID)
	if err != nil {
		r.log.Warn("topology: library row", "library", lib.Library.ID, "err", err)
		return
	}
	t := catalog.Topology{
		TopologyVersion: catalog.TopologyVersion,
		SystemName:      system, InstanceUUID: uuid,
		LibrarySerial: serial, LibraryName: row.Name,
		Product: row.Product, DriveProduct: row.DriveModel,
		NumDrives: row.NumDrives, NumSlots: lib.Library.NumSlots, NumMAP: lib.Library.NumIE,
		LabelPrefix: row.LabelPrefix,
		WrittenAt:   time.Now().UTC().Format(time.RFC3339), OpenvtldVersion: r.version,
	}
	body, err := t.Encode()
	if err != nil {
		return
	}
	if err := cl.PutFile(ctx, cl.Key(catalog.TopologyKeyParts(system, serial)...),
		bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
		r.log.Warn("topology write", "err", err)
	}
}

// runExport drives: detected -> quiescing -> chunking <-> uploading ->
// verifying -> unvaulting -> done. Resume re-enters at quiescing and
// skips chunks already in the ledger.
func (r *Runner) runExport(ctx context.Context, j *store.Job) error {
	if j.RemoteID == nil {
		return fmt.Errorf("export job has no remote")
	}
	remote, err := r.db.GetRemote(ctx, *j.RemoteID)
	if err != nil {
		return fmt.Errorf("remote %d: %w", *j.RemoteID, err)
	}
	cl, err := s3.New(remote)
	if err != nil {
		return err
	}

	// S3 key coordinates (v2 namespaced layout): this instance's system
	// name + the cart's library serial. The system marker is written once.
	system, uuid := r.systemIdentity(ctx)
	library := r.libSerialFor(j.CartLabel)
	if err := r.ensureSystemMarker(ctx, cl, system, uuid); err != nil {
		r.log.Warn("system marker write", "err", err)
	}
	r.ensureTopology(ctx, cl, system, uuid, library, j.CartLabel)

	if err := r.transition(ctx, j, "quiescing", "verifying cart is idle and out of any drive"); err != nil {
		return err
	}
	if err := r.quiesceCheck(j.CartLabel); err != nil {
		return err
	}
	dir := filepath.Join(r.mediaDirFor(j.CartLabel), j.CartLabel)
	if _, err := os.Stat(filepath.Join(dir, markerFile)); err == nil {
		return fmt.Errorf("cart %s is an evicted stub — nothing to export", j.CartLabel)
	}
	files, err := listCartFiles(dir)
	if err != nil {
		return err
	}
	var sumBytes int64
	for _, fi := range files {
		sumBytes += fi.Size()
	}

	if j.Generation == "" {
		j.Generation = catalog.NewGeneration(time.Now())
		if err := r.db.SetJobGeneration(ctx, j.ID, j.Generation); err != nil {
			return err
		}
	}

	// Resume point: contiguous uploaded chunks from 0. A different
	// cart size than last attempt means the media changed between
	// runs — the deterministic-tar assumption is void, start over.
	ledger, err := r.db.ChunksForJob(ctx, j.ID)
	if err != nil {
		return err
	}
	skipUntil := 0
	for _, c := range ledger {
		if c.Idx == skipUntil && c.UploadedAt != "" {
			skipUntil++
		} else {
			break
		}
	}
	if skipUntil > 0 && j.BytesTotal != sumBytes {
		r.log.Warn("cart changed since last attempt; restarting chunks", "job", j.ID)
		skipUntil = 0
		if err := r.db.ClearChunks(ctx, j.ID); err != nil {
			return err
		}
	}
	if err := r.db.SetJobTotals(ctx, j.ID, sumBytes, int((sumBytes+r.chunkBytes-1)/r.chunkBytes)); err != nil {
		return err
	}
	j.BytesTotal = sumBytes

	detail := fmt.Sprintf("generation %s, %d files, %s raw", j.Generation, len(files), human(sumBytes))
	if skipUntil > 0 {
		detail += fmt.Sprintf(" (resuming after chunk %d)", skipUntil-1)
	}
	if err := r.transition(ctx, j, "chunking", detail); err != nil {
		return err
	}

	var doneRaw int64
	sink := func(c chunkResult) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		key := cl.Key(catalog.ChunkKeyParts(system, library, j.CartLabel, j.Generation, c.Idx)...)
		if err := r.transition(ctx, j, "uploading",
			fmt.Sprintf("chunk %d: %s raw -> %s zstd", c.Idx, human(c.RawBytes), human(c.StoredBytes))); err != nil {
			return err
		}
		f, err := os.Open(c.StagePath)
		if err != nil {
			return err
		}
		err = cl.PutFile(ctx, key, f, c.StoredBytes, "application/zstd")
		f.Close()
		if err != nil {
			return err
		}
		if err := r.db.RecordChunk(ctx, store.ExportChunk{
			JobID: j.ID, Idx: c.Idx, S3Key: key,
			RawBytes: c.RawBytes, StoredBytes: c.StoredBytes, SHA256: c.SHA256,
		}); err != nil {
			return err
		}
		doneRaw += c.RawBytes
		r.progress(ctx, j.ID, doneRaw, c.Idx+1)
		return r.transition(ctx, j, "chunking", fmt.Sprintf("chunk %d uploaded", c.Idx))
	}

	ck := newChunker(ctx, r.chunkBytes, r.stagingDir, skipUntil, sink)
	cartFiles, err := writeCartTar(ck, dir, files)
	if err != nil {
		ck.abort()
		return err
	}
	if err := ck.Close(); err != nil {
		return err
	}
	chunksTotal := ck.idx
	if ck.curRaw > 0 {
		chunksTotal++
	}
	logicalBytes := ck.rawSeen
	if err := r.db.SetJobTotals(ctx, j.ID, logicalBytes, chunksTotal); err != nil {
		return err
	}
	r.progress(ctx, j.ID, logicalBytes, chunksTotal)

	if err := r.transition(ctx, j, "verifying",
		fmt.Sprintf("checking %d objects in bucket %s", chunksTotal, remote.Bucket)); err != nil {
		return err
	}
	ledger, err = r.db.ChunksForJob(ctx, j.ID)
	if err != nil {
		return err
	}
	if len(ledger) != chunksTotal {
		return fmt.Errorf("ledger has %d chunks, pipeline produced %d", len(ledger), chunksTotal)
	}
	var mChunks []catalog.Chunk
	var storedTotal int64
	for _, c := range ledger {
		oi, err := cl.Stat(ctx, c.S3Key)
		if err != nil {
			return fmt.Errorf("verify chunk %d: %w", c.Idx, err)
		}
		if oi.Size != c.StoredBytes {
			return fmt.Errorf("verify chunk %d: bucket has %d bytes, ledger says %d", c.Idx, oi.Size, c.StoredBytes)
		}
		mChunks = append(mChunks, catalog.Chunk{
			Idx: c.Idx, Key: c.S3Key, RawBytes: c.RawBytes,
			StoredBytes: c.StoredBytes, SHA256: c.SHA256,
		})
		storedTotal += c.StoredBytes
	}

	m := &catalog.Manifest{
		ManifestVersion: catalog.ManifestVersion,
		Label:           j.CartLabel,
		Generation:      j.Generation,
		ExportedAt:      time.Now().UTC().Format(time.RFC3339),
		Source: catalog.Source{
			SystemName: system, InstanceUUID: uuid,
			LibrarySerial: library, Hostname: r.hostname, OpenvtldVersion: r.version,
		},
		Format:    catalog.Format{Archive: "tar", Compression: "zstd", ChunkRawBytes: r.chunkBytes},
		CartFiles: cartFiles,
		Chunks:    mChunks,
		Totals: catalog.Totals{
			LogicalBytes: logicalBytes, StoredBytes: storedTotal, ChunkCount: chunksTotal,
		},
	}
	mb, err := m.Encode()
	if err != nil {
		return err
	}
	mKey := cl.Key(catalog.ManifestKeyParts(system, library, j.CartLabel, j.Generation)...)
	if err := cl.PutFile(ctx, mKey, bytes.NewReader(mb), int64(len(mb)), "application/json"); err != nil {
		return fmt.Errorf("manifest upload: %w", err)
	}
	if err := r.db.UpsertCatalogEntry(ctx, store.CatalogEntry{
		RemoteID: *j.RemoteID, SystemName: system, LibrarySerial: library,
		CartLabel: j.CartLabel, Generation: j.Generation,
		ManifestJSON: string(mb), LogicalBytes: logicalBytes,
		StoredBytes: storedTotal, ChunkCount: chunksTotal, ExportedAt: m.ExportedAt,
	}); err != nil {
		return err
	}
	if err := r.db.SetCartLocalState(ctx, j.CartLabel, "resident", j.Generation); err != nil {
		return err
	}

	if err := r.transition(ctx, j, "unvaulting", "returning cart to home slot if vaulted"); err != nil {
		return err
	}
	moveDetail, err := r.unvault(ctx, j.CartLabel)
	if err != nil {
		return fmt.Errorf("unvault: %w", err)
	}

	if err := r.transition(ctx, j, "done", moveDetail); err != nil {
		return err
	}
	r.db.LogEvent(ctx, time.Now(), "export_done", j.CartLabel,
		fmt.Sprintf("generation %s: %s raw -> %s in s3://%s (%d chunks); %s",
			j.Generation, human(logicalBytes), human(storedTotal), remote.Bucket, chunksTotal, moveDetail))
	return nil
}

func human(n int64) string {
	const u = 1024
	switch {
	case n >= u*u*u:
		return fmt.Sprintf("%.1f GiB", float64(n)/(u*u*u))
	case n >= u*u:
		return fmt.Sprintf("%.1f MiB", float64(n)/(u*u))
	case n >= u:
		return fmt.Sprintf("%.1f KiB", float64(n)/u)
	}
	return fmt.Sprintf("%d B", n)
}
