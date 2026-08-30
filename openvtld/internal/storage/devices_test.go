package storage

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// Canned lsblk -J -b shapes: an OS disk (mounted root in a child), an
// LVM member, and a bare disk.
const cannedLsblk = `{"blockdevices": [
  {"path":"/dev/sda","type":"disk","size":53687091200,"fstype":null,"pttype":"gpt","mountpoint":null,"model":"QEMU HARDDISK","tran":"sata","rota":true,
   "children":[{"path":"/dev/sda2","type":"part","size":49912754176,"fstype":"ext4","pttype":null,"mountpoint":"/","model":null,"tran":null,"rota":true}]},
  {"path":"/dev/sdb","type":"disk","size":53687091200,"fstype":"LVM2_member","pttype":null,"mountpoint":null,"model":"QEMU HARDDISK","tran":"sata","rota":true},
  {"path":"/dev/sdd","type":"disk","size":107374182400,"fstype":null,"pttype":null,"mountpoint":null,"model":"QEMU HARDDISK","tran":"sata","rota":true}
]}`

func TestTreeStateEligibility(t *testing.T) {
	var tree struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(cannedLsblk), &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree.Blockdevices) != 3 {
		t.Fatalf("parsed %d devices", len(tree.Blockdevices))
	}
	// sda: gpt signature + mounted root in the tree -> os disk
	hasSig, _, mounted, _, hasRoot := treeState(tree.Blockdevices[0])
	if !hasSig || !mounted || !hasRoot {
		t.Fatalf("sda: sig=%v mounted=%v root=%v", hasSig, mounted, hasRoot)
	}
	// sdb: LVM2_member signature, nothing mounted
	hasSig, sig, mounted, _, hasRoot := treeState(tree.Blockdevices[1])
	if !hasSig || sig != "LVM2_member" || mounted || hasRoot {
		t.Fatalf("sdb: sig=%v %q mounted=%v root=%v", hasSig, sig, mounted, hasRoot)
	}
	// sdd: bare
	hasSig, _, mounted, _, hasRoot = treeState(tree.Blockdevices[2])
	if hasSig || mounted || hasRoot {
		t.Fatalf("sdd should be bare: sig=%v mounted=%v root=%v", hasSig, mounted, hasRoot)
	}
}

func TestPoolNameRe(t *testing.T) {
	for _, ok := range []string{"pool1", "p_2", "aa"} {
		if !poolNameRe.MatchString(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"Pool1", "1pool", "a", "with-dash", "x y", ""} {
		if poolNameRe.MatchString(bad) {
			t.Errorf("%q should be rejected (dm names join on dashes)", bad)
		}
	}
}

// TestDevicesLive runs real lsblk — set OVTL_LIVE=1 on a target box.
func TestDevicesLive(t *testing.T) {
	if os.Getenv("OVTL_LIVE") == "" {
		t.Skip("OVTL_LIVE not set")
	}
	devs, err := Devices(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		t.Logf("%s size=%d model=%q eligible=%v role=%q reason=%q by_id=%s",
			d.Path, d.SizeBytes, d.Model, d.Eligible, d.Role, d.Reason, d.ByID)
	}
	if len(devs) == 0 {
		t.Fatal("no disks enumerated")
	}
}
