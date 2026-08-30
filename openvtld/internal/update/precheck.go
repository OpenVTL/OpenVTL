package update

import (
	"context"
	"fmt"
	"os"

	"github.com/openvtl/openvtld/internal/release"
)

// Precheck verifies a bundle's signature and runs the full preflight
// WITHOUT applying — the Updates panel's fast-fail path so a bad
// signature / same-version / downgrade / Tier-B bundle is refused in
// the upload response instead of minutes later in a journal. The
// detached apply re-runs both checks; they are read-only.
func Precheck(ctx context.Context, tarball string, opt Options) (release.Version, error) {
	opt.def()
	tmp, err := os.MkdirTemp("", "ovtl-precheck-")
	if err != nil {
		return release.Version{}, err
	}
	defer os.RemoveAll(tmp)
	root, err := unpackBundle(tarball, tmp)
	if err != nil {
		return release.Version{}, fmt.Errorf("unpack: %w", err)
	}
	if err := release.VerifyBundleDir(root); err != nil {
		return release.Version{}, fmt.Errorf("verify: %w", err)
	}
	bv, err := release.ParseVersionDir(root)
	if err != nil {
		return release.Version{}, fmt.Errorf("read bundle VERSION: %w", err)
	}
	return bv, preflight(ctx, opt.Paths, bv, opt)
}

// Status reports the pending-update marker and the last-known-good
// record (either may be absent) — the Updates panel's state.
func Status(p Paths) (pending, lastGood *Marker) {
	if m, ok := readMarker(p); ok {
		pending = &m
	}
	if m, ok := readLastGood(p); ok {
		lastGood = &m
	}
	return
}
