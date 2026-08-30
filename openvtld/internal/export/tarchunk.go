package export

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/openvtl/openvtld/internal/catalog"
)

// markerFile flags an evicted stub inside a cart dir. It is never
// included in exports and mhVTL ignores files it doesn't know.
const markerFile = ".openvtl-evicted.json"

// listCartFiles enumerates a cart dir's regular files, sorted by name.
// Sorting + truncated mtimes + PAX headers make the tar stream
// byte-deterministic for an unchanged dir — the property chunk-level
// resume depends on.
func listCartFiles(dir string) ([]os.FileInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []os.FileInfo
	for _, e := range ents {
		if !e.Type().IsRegular() || e.Name() == markerFile {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	if len(out) == 0 {
		return nil, fmt.Errorf("no media files in %s", dir)
	}
	return out, nil
}

func ownerIDs(fi os.FileInfo) (uid, gid int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return 0, 0
}

// writeCartTar streams the cart dir as a deterministic tar, hashing each
// file's content on the way through (single read pass). Returns manifest
// file entries in tar order.
//
// Headers are PAX, not USTAR: a real IBM i system-save cart's data.<n>
// file runs to hundreds of GB, and USTAR's 12-octal-digit size field
// tops out at 8 GiB (2026-07-05: exports of a 282 GiB cart failed with
// "USTAR cannot encode Size=..."). Go's PAX writer degrades to a
// byte-identical USTAR block when every field fits, so small files keep
// the deterministic layout resume relies on; only an oversized member
// gets an extended (size=) record — itself deterministic.
func writeCartTar(w io.Writer, dir string, files []os.FileInfo) ([]catalog.File, error) {
	tw := tar.NewWriter(w)
	var out []catalog.File
	for _, fi := range files {
		uid, gid := ownerIDs(fi)
		hdr := &tar.Header{
			Name:    fi.Name(),
			Size:    fi.Size(),
			Mode:    int64(fi.Mode().Perm()),
			ModTime: fi.ModTime().Truncate(time.Second),
			Uid:     uid,
			Gid:     gid,
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar header %s: %w", fi.Name(), err)
		}
		f, err := os.Open(filepath.Join(dir, fi.Name()))
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, h), f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("tar content %s: %w", fi.Name(), err)
		}
		if n != fi.Size() {
			return nil, fmt.Errorf("%s changed size during export (%d != %d) — cart not quiesced?", fi.Name(), n, fi.Size())
		}
		out = append(out, catalog.File{
			Name: fi.Name(), Size: fi.Size(), Mode: uint32(fi.Mode().Perm()),
			MTime: fi.ModTime().UTC().Format(time.RFC3339), SHA256: hex.EncodeToString(h.Sum(nil)),
		})
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// chunkResult hands one finished chunk to the uploader while its
// staging file still exists.
type chunkResult struct {
	Idx         int
	StagePath   string
	RawBytes    int64
	StoredBytes int64
	SHA256      string
}

// chunker splits the raw tar stream at fixed boundaries, compressing
// each window into its own zstd frame in a single staging file. Chunks
// below skipUntil are counted and discarded — that's resume: the
// deterministic tar is re-produced and already-uploaded windows skipped
// without compression cost.
type chunker struct {
	ctx         context.Context
	rawPerChunk int64
	stagingDir  string
	skipUntil   int
	sink        func(chunkResult) error

	idx     int
	curRaw  int64
	rawSeen int64

	f      *os.File
	enc    *zstd.Encoder
	sum    hash.Hash
	stored int64
}

func newChunker(ctx context.Context, rawPerChunk int64, stagingDir string, skipUntil int, sink func(chunkResult) error) *chunker {
	return &chunker{ctx: ctx, rawPerChunk: rawPerChunk, stagingDir: stagingDir, skipUntil: skipUntil, sink: sink}
}

type countWriter struct {
	w io.Writer
	n *int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

func (c *chunker) open() error {
	f, err := os.CreateTemp(c.stagingDir, "chunk-*.tar.zst.tmp")
	if err != nil {
		return fmt.Errorf("staging file: %w", err)
	}
	c.f = f
	c.sum = sha256.New()
	c.stored = 0
	enc, err := zstd.NewWriter(io.MultiWriter(countWriter{f, &c.stored}, c.sum),
		zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	c.enc = enc
	return nil
}

func (c *chunker) closeChunk() error {
	if err := c.enc.Close(); err != nil {
		return fmt.Errorf("zstd flush: %w", err)
	}
	path := c.f.Name()
	if err := c.f.Close(); err != nil {
		return err
	}
	c.f, c.enc = nil, nil
	res := chunkResult{
		Idx: c.idx, StagePath: path, RawBytes: c.curRaw,
		StoredBytes: c.stored, SHA256: hex.EncodeToString(c.sum.Sum(nil)),
	}
	err := c.sink(res)
	os.Remove(path) // staging file is consumed either way
	return err
}

func (c *chunker) Write(p []byte) (int, error) {
	// Honour cancellation on every write (io.Copy calls this ~every 32 KB):
	// the sink only checks once per completed chunk, and a resume re-reads
	// whole skipped chunks, so without this a cancel hangs for minutes
	// mid-chunk / during the re-read (a cancel was once ignored for
	// minutes while re-reading 80 GB of already-uploaded chunks).
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	total := len(p)
	for len(p) > 0 {
		room := c.rawPerChunk - c.curRaw
		n := int64(len(p))
		if n > room {
			n = room
		}
		part := p[:n]
		if c.idx >= c.skipUntil {
			if c.f == nil {
				if err := c.open(); err != nil {
					return total - len(p), err
				}
			}
			if _, err := c.enc.Write(part); err != nil {
				return total - len(p), err
			}
		}
		c.curRaw += n
		c.rawSeen += n
		p = p[n:]
		if c.curRaw == c.rawPerChunk {
			if c.idx >= c.skipUntil {
				if err := c.closeChunk(); err != nil {
					return total - len(p), err
				}
			}
			c.idx++
			c.curRaw = 0
		}
	}
	return total, nil
}

// Close flushes the final partial chunk.
func (c *chunker) Close() error {
	if c.curRaw > 0 && c.idx >= c.skipUntil {
		if c.f == nil {
			// Entire final chunk was skipped bytes; nothing staged.
			return nil
		}
		return c.closeChunk()
	}
	if c.f != nil { // boundary-aligned leftover shouldn't happen, but never leak
		c.enc.Close()
		path := c.f.Name()
		c.f.Close()
		os.Remove(path)
	}
	return nil
}

// abort releases resources after an error without invoking the sink.
func (c *chunker) abort() {
	if c.enc != nil {
		c.enc.Close()
	}
	if c.f != nil {
		path := c.f.Name()
		c.f.Close()
		os.Remove(path)
	}
}
