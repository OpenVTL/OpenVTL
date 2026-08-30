package export

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/openvtl/openvtld/internal/catalog"
)

// makeCartDir builds a fake cart dir with mhVTL-shaped files at odd
// sizes so chunk boundaries land mid-file.
func makeCartDir(t *testing.T, seed int64) string {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(seed))
	files := map[string]int{
		"data.0":     3_500_017, // spans several 1 MiB chunks
		"indx.0":     70_001,
		"mam":        1047,
		"meta.0":     528,
		"mhvtl_data": 87,
	}
	for name, size := range files {
		b := make([]byte, size)
		rng.Read(b)
		// Repeat a block so zstd has something to chew on.
		if size > 4096 {
			copy(b[size/2:], b[:2048])
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type memChunk struct {
	res  chunkResult
	data []byte
}

// runChunker tars dir through a chunker, capturing every produced chunk.
func runChunker(t *testing.T, dir string, rawPerChunk int64, skipUntil int) ([]catalog.File, []memChunk, int64) {
	t.Helper()
	staging := t.TempDir()
	var out []memChunk
	sink := func(c chunkResult) error {
		b, err := os.ReadFile(c.StagePath)
		if err != nil {
			return err
		}
		out = append(out, memChunk{res: c, data: b})
		return nil
	}
	ck := newChunker(context.Background(), rawPerChunk, staging, skipUntil, sink)
	files, err := listCartFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cartFiles, err := writeCartTar(ck, dir, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Close(); err != nil {
		t.Fatal(err)
	}
	// No staging leaks.
	ents, _ := os.ReadDir(staging)
	if len(ents) != 0 {
		t.Fatalf("staging dir not empty after run: %d files", len(ents))
	}
	return cartFiles, out, ck.rawSeen
}

func TestExportImportRoundTrip(t *testing.T) {
	dir := makeCartDir(t, 1)
	const rawPerChunk = 1 << 20

	cartFiles, chunks, rawTotal := runChunker(t, dir, rawPerChunk, 0)
	if len(chunks) < 3 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	var raw int64
	for _, c := range chunks {
		raw += c.res.RawBytes
		sum := sha256.Sum256(c.data)
		if hex.EncodeToString(sum[:]) != c.res.SHA256 {
			t.Fatalf("chunk %d staged sha != reported sha", c.res.Idx)
		}
		if int64(len(c.data)) != c.res.StoredBytes {
			t.Fatalf("chunk %d stored size mismatch", c.res.Idx)
		}
	}
	if raw != rawTotal {
		t.Fatalf("chunk raw sum %d != tar size %d", raw, rawTotal)
	}

	// Reassemble through chunkStream exactly as import does.
	var mChunks []catalog.Chunk
	byKey := map[string][]byte{}
	for _, c := range chunks {
		key := catalog.ChunkKeyParts("sys", "LIB", "TEST01L5", "20260703T000000Z", c.res.Idx)[4]
		mChunks = append(mChunks, catalog.Chunk{
			Idx: c.res.Idx, Key: key, RawBytes: c.res.RawBytes,
			StoredBytes: c.res.StoredBytes, SHA256: c.res.SHA256,
		})
		byKey[key] = c.data
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	cs := &chunkStream{chunks: mChunks, dec: dec, open: func(key string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(byKey[key])), nil
	}}
	tr := tar.NewReader(cs)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			t.Fatal(err)
		}
		got[hdr.Name] = hex.EncodeToString(h.Sum(nil))
	}
	if err := cs.finish(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cartFiles) {
		t.Fatalf("extracted %d files, want %d", len(got), len(cartFiles))
	}
	for _, cf := range cartFiles {
		if got[cf.Name] != cf.SHA256 {
			t.Fatalf("%s: round-trip hash mismatch", cf.Name)
		}
		// And against the source of truth on disk.
		b, err := os.ReadFile(filepath.Join(dir, cf.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != cf.SHA256 {
			t.Fatalf("%s: manifest hash != disk hash", cf.Name)
		}
	}
}

// TestLargeMemberHeaderEncodes pins the 2026-07-05 fix: a cart data
// file larger than USTAR's 8 GiB size ceiling (a real IBM i system save
// is hundreds of GB) must encode. PAX handles it; USTAR is proven to
// reject it so the regression can't silently return. Only the header is
// written — no need to stream 282 GiB of content.
func TestLargeMemberHeaderEncodes(t *testing.T) {
	const bigSize = 303085230096 // the exact byte count from the real-world failure (~282 GiB)

	hdr := func(f tar.Format) *tar.Header {
		return &tar.Header{Name: "data.0", Size: bigSize, Mode: 0o640, Format: f}
	}
	// PAX: encodes fine.
	tw := tar.NewWriter(io.Discard)
	if err := tw.WriteHeader(hdr(tar.FormatPAX)); err != nil {
		t.Fatalf("PAX must encode a %d-byte member, got: %v", bigSize, err)
	}
	// USTAR: still rejects it — guards against a regression back to USTAR.
	if err := tar.NewWriter(io.Discard).WriteHeader(hdr(tar.FormatUSTAR)); err == nil {
		t.Fatal("expected USTAR to reject a >8 GiB member (the original bug)")
	}
}

// TestChunkerCancels pins the 2026-07-06 fix: a cancelled context stops
// the chunker on the next write rather than churning through the rest of a
// chunk / a resume re-read (the cancel button appeared to "do
// nothing" for minutes).
func TestChunkerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ck := newChunker(ctx, 1<<20, t.TempDir(), 0, func(chunkResult) error { return nil })
	if _, err := ck.Write(make([]byte, 4096)); err != context.Canceled {
		t.Fatalf("cancelled chunker write returned %v, want context.Canceled", err)
	}
}

// TestResumeDeterminism proves the property chunk-level resume rests
// on: re-running the tar over an unchanged dir with skipUntil=N yields
// byte-identical chunks N.. as the original full run.
func TestResumeDeterminism(t *testing.T) {
	dir := makeCartDir(t, 2)
	const rawPerChunk = 1 << 20

	_, full, _ := runChunker(t, dir, rawPerChunk, 0)
	skip := 2
	if len(full) <= skip {
		t.Fatalf("need > %d chunks, got %d", skip, len(full))
	}
	_, resumed, _ := runChunker(t, dir, rawPerChunk, skip)
	if len(resumed) != len(full)-skip {
		t.Fatalf("resume produced %d chunks, want %d", len(resumed), len(full)-skip)
	}
	for i, rc := range resumed {
		fc := full[skip+i]
		if rc.res.Idx != fc.res.Idx {
			t.Fatalf("chunk index mismatch: %d vs %d", rc.res.Idx, fc.res.Idx)
		}
		if rc.res.SHA256 != fc.res.SHA256 || !bytes.Equal(rc.data, fc.data) {
			t.Fatalf("chunk %d not byte-identical on resume", rc.res.Idx)
		}
	}
}

// TestChunkBoundaryExact covers a tar stream that is an exact multiple
// of the chunk size (the empty-final-chunk edge).
func TestChunkBoundaryExact(t *testing.T) {
	dir := t.TempDir()
	// tar = 512 hdr + data padded to 512 + 2*512 trailer. Make total
	// exactly 2 chunks of 1536 bytes: file of 1024+512=1536? total =
	// 512 + 1536 + 1024 = 3072 = 2*1536.
	b := make([]byte, 1536)
	rand.New(rand.NewSource(3)).Read(b)
	if err := os.WriteFile(filepath.Join(dir, "data.0"), b, 0o640); err != nil {
		t.Fatal(err)
	}
	_, chunks, rawTotal := runChunker(t, dir, 1536, 0)
	if rawTotal != 3072 {
		t.Skipf("tar layout differs (%d bytes), edge not hit", rawTotal)
	}
	if len(chunks) != 2 {
		t.Fatalf("want exactly 2 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.res.RawBytes != 1536 {
			t.Fatalf("chunk %d raw %d != 1536", c.res.Idx, c.res.RawBytes)
		}
	}
}
