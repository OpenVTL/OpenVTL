package catalog

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openvtl/openvtld/internal/s3"
	"github.com/openvtl/openvtld/internal/store"
)

// RebuildResult reports one catalog rebuild pass.
type RebuildResult struct {
	Complete   int          `json:"complete"`   // manifests fetched + cached
	Incomplete []Incomplete `json:"incomplete"` // generation dirs with no manifest (aborted exports)
	Errors     []string     `json:"errors,omitempty"`
}

type Incomplete struct {
	System     string `json:"system"`
	Library    string `json:"library"`
	Label      string `json:"label"`
	Generation string `json:"generation"`
}

// Rebuild wipes the cached catalog for a remote and repopulates it from
// the bucket listing alone — the "fresh openvtld" recovery path and the
// periodic sync. Manifest-less generation dirs are reported, not cached.
func Rebuild(ctx context.Context, db *store.Store, cl *s3.Client, remoteID int64, log *slog.Logger) (*RebuildResult, error) {
	gens, err := cl.ListGenerations(ctx)
	if err != nil {
		return nil, fmt.Errorf("bucket listing: %w", err)
	}
	// Non-nil slice: an empty result must marshal as [] not null, or the
	// UI's rebuild.incomplete.length crashes the view (array-null lesson).
	res := &RebuildResult{Incomplete: []Incomplete{}}
	if err := db.ClearCatalog(ctx, remoteID); err != nil {
		return nil, fmt.Errorf("catalog clear: %w", err)
	}
	for _, g := range gens {
		id := g.System + "/" + g.Library + "/" + g.Label + "/" + g.Generation
		if !g.HasManifest {
			res.Incomplete = append(res.Incomplete, Incomplete{g.System, g.Library, g.Label, g.Generation})
			continue
		}
		raw, err := cl.GetBytes(ctx, cl.Key(ManifestKeyParts(g.System, g.Library, g.Label, g.Generation)...))
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		m, err := Decode(raw)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		// The manifest must self-identify as the same coordinates as its key.
		if m.Label != g.Label || m.Generation != g.Generation ||
			m.Source.SystemName != g.System || m.Source.LibrarySerial != g.Library {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: manifest self-identifies as %s/%s/%s/%s",
				id, m.Source.SystemName, m.Source.LibrarySerial, m.Label, m.Generation))
			continue
		}
		if err := db.UpsertCatalogEntry(ctx, store.CatalogEntry{
			RemoteID: remoteID, SystemName: m.Source.SystemName, LibrarySerial: m.Source.LibrarySerial,
			CartLabel: m.Label, Generation: m.Generation,
			ManifestJSON: string(raw), LogicalBytes: m.Totals.LogicalBytes,
			StoredBytes: m.Totals.StoredBytes, ChunkCount: m.Totals.ChunkCount,
			ExportedAt: m.ExportedAt,
		}); err != nil {
			return nil, fmt.Errorf("catalog upsert %s: %w", id, err)
		}
		res.Complete++
	}
	log.Info("catalog rebuilt", "remote", remoteID,
		"complete", res.Complete, "incomplete", len(res.Incomplete), "errors", len(res.Errors))
	return res, nil
}
