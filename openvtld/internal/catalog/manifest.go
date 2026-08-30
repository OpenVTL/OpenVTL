// Package catalog defines the export manifest — the self-describing
// catalog of record stored in S3 — and rebuilds the local cache from a
// bucket listing. A fresh openvtld pointed at a bucket must reconstruct
// everything importable from manifests alone; SQLite is only a cache.
package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ManifestVersion 2 (2026-07-06): keys are namespaced
// system/library/label/generation and the source carries the system
// identity. v1 (flat label/generation) manifests do not decode — the
// layout change ships with a clean-bucket cutover.
const ManifestVersion = 2

// Manifest describes one exported generation of one cartridge. It is
// uploaded LAST: its presence marks the export complete, so a bare
// bucket listing distinguishes finished exports from aborted ones.
type Manifest struct {
	ManifestVersion int     `json:"manifest_version"`
	Label           string  `json:"label"`
	Generation      string  `json:"generation"`
	ExportedAt      string  `json:"exported_at"`
	Source          Source  `json:"source"`
	Format          Format  `json:"format"`
	CartFiles       []File  `json:"cart_files"`
	Chunks          []Chunk `json:"chunks"`
	Totals          Totals  `json:"totals"`
}

type Source struct {
	SystemName      string `json:"system_name"`   // friendly instance name (path segment)
	InstanceUUID    string `json:"instance_uuid"` // stable, collision-proof backstop
	LibrarySerial   string `json:"library_serial"`
	Hostname        string `json:"hostname"`
	OpenvtldVersion string `json:"openvtld_version"`
}

type Format struct {
	Archive       string `json:"archive"`     // "tar"
	Compression   string `json:"compression"` // "zstd"
	ChunkRawBytes int64  `json:"chunk_raw_bytes"`
}

// File records one cart file inside the tar (data.N/indx.N/meta.N/mam)
// with its content hash — the byte-identical round-trip check.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	MTime  string `json:"mtime"`
	SHA256 string `json:"sha256"`
}

// Chunk is one independently-decompressible zstd frame of the tar
// stream, cut at a fixed raw (uncompressed) boundary.
type Chunk struct {
	Idx         int    `json:"idx"`
	Key         string `json:"key"` // full object key incl. remote prefix
	RawBytes    int64  `json:"raw_bytes"`
	StoredBytes int64  `json:"stored_bytes"`
	SHA256      string `json:"sha256"` // of the compressed object
}

type Totals struct {
	LogicalBytes int64 `json:"logical_bytes"` // tar stream size
	StoredBytes  int64 `json:"stored_bytes"`  // sum of compressed chunks
	ChunkCount   int   `json:"chunk_count"`
}

func (m *Manifest) Encode() ([]byte, error) {
	return json.MarshalIndent(m, "", " ")
}

func Decode(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if m.ManifestVersion != ManifestVersion {
		return nil, fmt.Errorf("manifest version %d not supported (want %d)", m.ManifestVersion, ManifestVersion)
	}
	if m.Label == "" || m.Generation == "" || len(m.Chunks) == 0 {
		return nil, fmt.Errorf("manifest incomplete: label=%q generation=%q chunks=%d", m.Label, m.Generation, len(m.Chunks))
	}
	return &m, nil
}

// Generation keys are compact UTC timestamps — sortable, S3-safe, and
// unique per (label, export) since a cart exports at most once at a time.
const genLayout = "20060102T150405Z"

var genRe = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

func NewGeneration(t time.Time) string { return t.UTC().Format(genLayout) }

func ValidGeneration(g string) bool { return genRe.MatchString(g) }

// Object key layout under the remote prefix (v2, System > Library > Tape):
//
//	<system>/<library>/<label>/<generation>/manifest.json
//	<system>/<library>/<label>/<generation>/chunk-00000.tar.zst
//	<system>/.openvtl-system.json
//
// The system (instance) segment lets instances share one bucket without
// label collision; the library is the MLB serial (immutable, unlike the
// friendly library name); the tape is the cartridge label.
func ManifestKeyParts(system, library, label, gen string) []string {
	return []string{system, library, label, gen, "manifest.json"}
}

func ChunkKeyParts(system, library, label, gen string, idx int) []string {
	return []string{system, library, label, gen, fmt.Sprintf("chunk-%05d.tar.zst", idx)}
}

// SystemMarker sits at <system>/.openvtl-system.json so any instance
// rebuilding the bucket can enumerate systems and detect a friendly name
// already owned by a different instance UUID (a clash, or a deliberate
// identity adoption during scratch recovery).
type SystemMarker struct {
	SystemName   string `json:"system_name"`
	InstanceUUID string `json:"instance_uuid"`
	CreatedAt    string `json:"created_at"`
}

func SystemMarkerKeyParts(system string) []string {
	return []string{system, ".openvtl-system.json"}
}

// systemNameRe keeps the friendly system name path-safe.
var systemNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func ValidSystemName(s string) bool { return systemNameRe.MatchString(s) }

// SanitizeName reduces a hostname (or any string) to a path-safe system
// name — the default when the operator hasn't set one.
func SanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == '.' || r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-_")
	}
	if out == "" {
		return "openvtl"
	}
	return out
}
