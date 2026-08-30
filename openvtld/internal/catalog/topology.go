package catalog

import (
	"encoding/json"
	"fmt"
)

// TopologyVersion 1: a per-library descriptor stored beside a library's tape
// subtrees at <system>/<library-serial>/topology.json. It lets a fresh box
// recreate the library (model, drive model, drive/slot/MAP counts, label
// prefix) before importing its carts — the basis of one-click "Recover
// library". At 3 key segments it is skipped by ListGenerations (which wants
// 5), so it never shows up as a bogus generation in the catalog.
const TopologyVersion = 1

type Topology struct {
	TopologyVersion int    `json:"topology_version"`
	SystemName      string `json:"system_name"`
	InstanceUUID    string `json:"instance_uuid"`
	LibrarySerial   string `json:"library_serial"`
	LibraryName     string `json:"library_name"` // friendly display name
	Product         string `json:"product"`      // library model id (3573-TL, …)
	DriveProduct    string `json:"drive_product"`
	NumDrives       int    `json:"num_drives"`
	NumSlots        int    `json:"num_slots"`
	NumMAP          int    `json:"num_map"`
	LabelPrefix     string `json:"label_prefix"`
	WrittenAt       string `json:"written_at"`
	OpenvtldVersion string `json:"openvtld_version"`
}

func TopologyKeyParts(system, serial string) []string {
	return []string{system, serial, "topology.json"}
}

func (t *Topology) Encode() ([]byte, error) { return json.MarshalIndent(t, "", " ") }

func DecodeTopology(b []byte) (*Topology, error) {
	var t Topology
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("topology parse: %w", err)
	}
	if t.TopologyVersion != TopologyVersion {
		return nil, fmt.Errorf("topology version %d not supported (want %d)", t.TopologyVersion, TopologyVersion)
	}
	if t.Product == "" || t.DriveProduct == "" || t.NumDrives < 1 {
		return nil, fmt.Errorf("topology incomplete: product=%q drive=%q drives=%d", t.Product, t.DriveProduct, t.NumDrives)
	}
	return &t, nil
}
