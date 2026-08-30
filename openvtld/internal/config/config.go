// Package config holds openvtld runtime configuration.
//
// v0.3 is flag-driven (the systemd unit carries site specifics); the
// settings table takes over for UI-editable values in v0.5.
package config

import (
	"flag"
	"time"
)

type Config struct {
	Listen    string // optional plaintext listen address (metrics/health + redirect); "" disables
	ListenTLS string // HTTPS listen address (the real API)
	TLSDir    string // self-signed cert/key directory
	DBPath    string // SQLite database file
	MhvtlConf string // mhVTL config dir (/etc/mhvtl)
	MediaDir  string // cartridge pool mount (/opt/mhvtl)

	PollChanger  time.Duration // element-status poll interval
	ScanMedia    time.Duration // media-dir scan interval
	PollStats    time.Duration // pool stats interval
	JournalSince string        // journalctl --since for startup backfill

	ACLs   string // comma-separated initiator WWPNs (one-time seed; DB is authority)
	VDOLV  string // legacy single-pool stats fallback (pre-v0.6 installs); unused on current systems
	SkipFC bool   // skip FC target verify/rebuild at startup

	StagingDir string // export chunk staging (OS disk, one chunk at a time)
	ChunkBytes int64  // raw bytes per export chunk

	ShowVersion bool // print version and exit
}

func Load() *Config {
	c := &Config{}
	flag.StringVar(&c.Listen, "listen", ":8080", "optional plaintext listen address (metrics/health + HTTPS redirect); empty disables")
	flag.StringVar(&c.ListenTLS, "listen-tls", ":8443", "HTTPS listen address")
	flag.StringVar(&c.TLSDir, "tls-dir", "/var/lib/openvtld/tls", "TLS certificate directory (self-signed pair generated on first run)")
	flag.StringVar(&c.DBPath, "db", "/var/lib/openvtld/openvtld.db", "SQLite database path")
	flag.StringVar(&c.MhvtlConf, "mhvtl-conf", "/etc/mhvtl", "mhVTL configuration directory")
	flag.StringVar(&c.MediaDir, "media-dir", "/opt/mhvtl", "cartridge pool directory")
	flag.DurationVar(&c.PollChanger, "poll-changer", 5*time.Second, "changer element-status poll interval")
	flag.DurationVar(&c.ScanMedia, "scan-media", 20*time.Second, "media directory scan interval")
	flag.DurationVar(&c.PollStats, "poll-stats", 10*time.Second, "pool statistics poll interval")
	flag.StringVar(&c.JournalSince, "journal-since", "-5m", "journal backfill window at startup")
	flag.StringVar(&c.ACLs, "acls", "", "initiator WWPN ACLs to seed once into the DB (comma-separated; the UI manages them thereafter)")
	flag.StringVar(&c.VDOLV, "vdo-lv", "", "legacy single-pool stats fallback (pre-v0.6 installs); unused on current systems")
	flag.BoolVar(&c.SkipFC, "skip-fc", false, "skip FC target verification at startup")
	flag.StringVar(&c.StagingDir, "staging-dir", "/var/lib/openvtld/staging", "export chunk staging directory")
	flag.Int64Var(&c.ChunkBytes, "chunk-bytes", 10<<30, "raw bytes per export chunk")
	flag.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	flag.Parse()
	return c
}
