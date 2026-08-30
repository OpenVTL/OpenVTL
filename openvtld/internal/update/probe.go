package update

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Health is what the /healthz probe extracts: liveness plus the version now
// serving (so the updater can tell the NEW binary apart from a rolled-back old
// one).
type Health struct {
	OK      bool
	Version string
}

// probe hits the local health endpoint — plaintext :8080 first (no TLS), then
// the self-signed HTTPS port. Returns the health payload and whether any
// endpoint answered.
func probe(ctx context.Context, p Paths) (Health, bool) {
	var urls []string
	if p.Plain != "" {
		urls = append(urls, "http://"+p.Plain+"/healthz")
	}
	if p.TLS != "" {
		urls = append(urls, "https://"+p.TLS+"/healthz")
	}
	for _, u := range urls {
		if h, ok := probeURL(ctx, u); ok {
			return h, true
		}
	}
	return Health{}, false
}

func probeURL(ctx context.Context, url string) (Health, bool) {
	c := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			// The API's cert is self-signed; we only care that the LOCAL daemon
			// answers, not who it is.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			DialContext:     (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Health{}, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return Health{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Health{}, false
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return Health{}, false
	}
	return Health{OK: body.OK, Version: body.Version}, true
}

// waitHealthy polls until an endpoint reports OK (optionally with a specific
// version) or the deadline passes. Returns the last health seen and whether the
// wait succeeded.
func waitHealthy(ctx context.Context, p Paths, wantVersion string, deadline time.Duration) (Health, bool) {
	end := time.Now().Add(deadline)
	for {
		if h, ok := probe(ctx, p); ok && h.OK {
			if wantVersion == "" || h.Version == wantVersion {
				return h, true
			}
		}
		if time.Now().After(end) {
			h, _ := probe(ctx, p)
			return h, false
		}
		select {
		case <-ctx.Done():
			return Health{}, false
		case <-time.After(2 * time.Second):
		}
	}
}

// buildNewer reports whether bundle build time is strictly newer than current.
// comparable is false when either timestamp is missing/unparseable (a dev binary
// has no embedded buildDate) — the caller then skips downgrade enforcement.
func buildNewer(bundleBuild, curBuild string) (newer, comparable bool) {
	bt, err1 := time.Parse(time.RFC3339, strings.TrimSpace(bundleBuild))
	ct, err2 := time.Parse(time.RFC3339, strings.TrimSpace(curBuild))
	if err1 != nil || err2 != nil {
		return false, false
	}
	return bt.After(ct), true
}

// mhvtlPinChanged compares the bundle's vendored mhVTL pin against the installed
// one (install.sh writes it to /usr/src/openvtl-mhvtl/.openvtl-pin, without the
// "mhvtl-" prefix). A change means the update touches Tier B and the updater
// must refuse. known is false when the installed pin can't be read.
func mhvtlPinChanged(bundlePin, installedPinFile string) (changed, known bool) {
	b, err := os.ReadFile(installedPinFile)
	if err != nil {
		return false, false
	}
	installed := strings.TrimSpace(string(b))
	want := strings.TrimPrefix(strings.TrimSpace(bundlePin), "mhvtl-")
	if want == "" || installed == "" {
		return false, false
	}
	return want != installed, true
}
