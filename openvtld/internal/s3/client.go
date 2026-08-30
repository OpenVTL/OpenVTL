// Package s3 wraps minio-go for the export/import pipeline. One Client
// per configured remote; credentials come from the s3_remote table and
// never appear in logs or errors.
package s3

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/openvtl/openvtld/internal/store"
)

type Client struct {
	mc     *minio.Client
	bucket string
	prefix string // normalized: "" or "path/" (trailing slash, no leading)
}

func New(r *store.Remote) (*Client, error) {
	mc, err := minio.New(r.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(r.AccessKey, r.SecretKey, ""),
		Secure:       r.UseSSL,
		Region:       r.Region,
		BucketLookup: lookup(r.PathStyle),
		// SHA-256 upload checksums ride in trailing headers.
		TrailingHeaders: true,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client init: %w", err)
	}
	return &Client{mc: mc, bucket: r.Bucket, prefix: normPrefix(r.Prefix)}, nil
}

func lookup(pathStyle bool) minio.BucketLookupType {
	if pathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupAuto
}

func normPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Key builds a full object key under the remote's prefix.
func (c *Client) Key(parts ...string) string {
	return c.prefix + strings.Join(parts, "/")
}

// Test verifies connectivity and permissions: bucket exists, we can
// write, read back, and delete a probe object under the prefix.
func (c *Client) Test(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return "", fmt.Errorf("bucket check: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("bucket %q not found (or no permission)", c.bucket)
	}
	probe := c.Key(".openvtl-probe")
	body := []byte("openvtl connectivity probe " + time.Now().UTC().Format(time.RFC3339))
	if _, err := c.mc.PutObject(ctx, c.bucket, probe,
		strings.NewReader(string(body)), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		return "", fmt.Errorf("probe write: %w", err)
	}
	obj, err := c.mc.GetObject(ctx, c.bucket, probe, minio.GetObjectOptions{})
	if err == nil {
		_, err = io.ReadAll(obj)
		obj.Close()
	}
	if err != nil {
		return "", fmt.Errorf("probe read: %w", err)
	}
	if err := c.mc.RemoveObject(ctx, c.bucket, probe, minio.RemoveObjectOptions{}); err != nil {
		return "", fmt.Errorf("probe delete: %w", err)
	}
	return fmt.Sprintf("bucket %q reachable; write/read/delete verified", c.bucket), nil
}

// PutFile uploads from a reader with known size, sending a SHA-256
// checksum so S3 verifies integrity on ingest.
func (c *Client) PutFile(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := c.mc.PutObject(ctx, c.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:    contentType,
		Checksum:       minio.ChecksumSHA256,
		SendContentMd5: false,
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

type ObjectInfo struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// Stat returns object size, or an error if missing — the verify step.
func (c *Client) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	oi, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", key, err)
	}
	return &ObjectInfo{Key: oi.Key, Size: oi.Size}, nil
}

// Get streams an object.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	// GetObject is lazy; surface missing-object errors now.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return obj, nil
}

// GetBytes fetches a small object (manifests) fully into memory.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	rc, err := c.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Generation is one <system>/<library>/<label>/<generation> directory
// found in the bucket (v2 namespaced layout).
type Generation struct {
	System      string
	Library     string
	Label       string
	Generation  string
	HasManifest bool
}

// ListGenerations walks the bucket under the prefix and returns every
// <system>/<library>/<label>/<generation>/ directory, noting whether
// manifest.json is present — the basis for catalog rebuild (across ALL
// systems sharing the bucket) and incomplete-export detection. Non-
// matching keys (the <system>/.openvtl-system.json markers, or legacy
// flat keys from before the cutover) are skipped.
func (c *Client) ListGenerations(ctx context.Context) ([]Generation, error) {
	seen := map[string]*Generation{} // "system/library/label/gen" -> entry
	opts := minio.ListObjectsOptions{Prefix: c.prefix, Recursive: true}
	for obj := range c.mc.ListObjects(ctx, c.bucket, opts) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list: %w", obj.Err)
		}
		rel := strings.TrimPrefix(obj.Key, c.prefix)
		parts := strings.SplitN(rel, "/", 5)
		// <system>/<library>/<label>/<generation>/<file>
		if len(parts) < 5 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
			continue
		}
		id := strings.Join(parts[:4], "/")
		g, ok := seen[id]
		if !ok {
			g = &Generation{System: parts[0], Library: parts[1], Label: parts[2], Generation: parts[3]}
			seen[id] = g
		}
		if parts[4] == "manifest.json" {
			g.HasManifest = true
		}
	}
	out := make([]Generation, 0, len(seen))
	for _, g := range seen {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return out[i].System < out[j].System
		}
		if out[i].Library != out[j].Library {
			return out[i].Library < out[j].Library
		}
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Generation < out[j].Generation
	})
	return out, nil
}

// Remove deletes one object.
func (c *Client) Remove(ctx context.Context, key string) error {
	return c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}

// ListAllObjects returns every object under the remote prefix, keyed
// RELATIVE to that prefix (system/library/label/generation/file) — the
// raw bucket browser.
func (c *Client) ListAllObjects(ctx context.Context) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for obj := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: c.prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list: %w", obj.Err)
		}
		out = append(out, ObjectInfo{Key: strings.TrimPrefix(obj.Key, c.prefix), Size: obj.Size})
	}
	return out, nil
}

// RemovePrefix deletes every object under a relative folder prefix and
// returns the count. The caller enforces folder-only semantics (never a
// lone chunk/manifest object).
func (c *Client) RemovePrefix(ctx context.Context, relPrefix string) (int, error) {
	full := c.prefix + relPrefix
	n := 0
	for obj := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: true}) {
		if obj.Err != nil {
			return n, fmt.Errorf("list: %w", obj.Err)
		}
		if err := c.mc.RemoveObject(ctx, c.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return n, fmt.Errorf("remove %s: %w", obj.Key, err)
		}
		n++
	}
	return n, nil
}
