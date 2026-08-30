package inventory

import (
	"slices"
	"sort"
	"testing"
)

// mktape's accepted density arguments on the installed build (mhvtl
// 1.8.0) — the catalog must never emit anything outside this set.
var mktapeDensities = map[string]bool{
	"AIT1": true, "AIT2": true, "AIT3": true, "AIT4": true,
	"DDS1": true, "DDS2": true, "DDS3": true, "DDS4": true,
	"DLT3": true, "DLT4": true,
	"SDLT1": true, "SDLT220": true, "SDLT320": true, "SDLT600": true,
	"LTO1": true, "LTO2": true, "LTO3": true, "LTO4": true,
	"LTO5": true, "LTO6": true, "LTO7": true, "LTO8": true,
	"T10KA": true, "T10KB": true, "T10KC": true,
	"9840A": true, "9840B": true, "9840C": true, "9840D": true,
	"9940A": true, "9940B": true,
	"J1A": true, "E05": true, "E06": true, "E07": true,
}

func TestCatalogInvariants(t *testing.T) {
	for _, d := range DriveModels() {
		if !mktapeDensities[d.Density] {
			t.Errorf("drive %s: density %q not accepted by mktape", d.Product, d.Density)
		}
		if d.Suffix == "" || d.Product == "" || d.Family == "" {
			t.Errorf("drive %s: incomplete entry %+v", d.Product, d)
		}
		got, ok := DriveModelByProduct(d.Product)
		if !ok || got.Density != d.Density {
			t.Errorf("DriveModelByProduct(%s) round-trip failed", d.Product)
		}
	}
	var creatable []string
	for _, m := range LibraryModels() {
		for _, v := range m.Variants {
			if v.Creatable {
				creatable = append(creatable, v.Product)
				if !m.Creatable {
					t.Errorf("variant %s creatable but parent %q is not", v.Product, m.Display)
				}
			}
		}
		if !m.Creatable {
			continue
		}
		if !m.IBMi {
			t.Errorf("library %q creatable but not IBMi-compatible", m.Display)
		}
		if len(m.Variants) == 0 {
			t.Errorf("library %q creatable with no variants", m.Display)
		}
		if m.MaxDrives < 1 {
			t.Errorf("library %q creatable with no drive cap", m.Display)
		}
		for _, v := range m.Variants {
			pm, pv, ok := LibraryVariantByProduct(v.Product)
			if !ok || pm.Display != m.Display || pv.Family != v.Family {
				t.Errorf("LibraryVariantByProduct(%s) round-trip failed", v.Product)
			}
			drives := CompatibleDrives(v)
			if len(drives) == 0 {
				t.Errorf("variant %s: no compatible drives in catalog", v.Product)
			}
			ibmi := 0
			for _, d := range drives {
				if d.Family != v.Family {
					t.Errorf("variant %s: incompatible drive %s offered", v.Product, d.Product)
				}
				if d.IBMi {
					ibmi++
				}
			}
			if ibmi == 0 {
				t.Errorf("variant %s: no IBMi-compatible drive available", v.Product)
			}
		}
	}
	if _, _, ok := LibraryVariantByProduct("3573-TL"); !ok {
		t.Error("the proven 3573-TL variant must exist")
	}
	if d, ok := DriveModelByProduct("ULT3580-TD5"); !ok || d.Density != "LTO5" || d.Suffix != "L5" {
		t.Errorf("ULT3580-TD5 entry wrong: %+v ok=%v", d, ok)
	}
	// Exactly the operator-approved creatable set (FC-only product,
	// 2026-08-24): the field-proven 3573-TL and the spec-validated
	// 03584L32. The 3584-403 iSCSI variant is retired.
	sort.Strings(creatable)
	if want := []string{"03584L32", "3573-TL"}; !slices.Equal(creatable, want) {
		t.Errorf("creatable variants %v, want %v", creatable, want)
	}
	// Frame families per the TS3500 operator manual: LTO frames are
	// x32/x52/x53, 3592 frames are x22/x23 (the pre-2026-07 catalog had
	// L22/D22 and L53/D53 backwards).
	for product, family := range map[string]string{
		"03584L32": "LTO", "03584L53": "LTO", "03584D53": "LTO",
		"03584L22": "3592", "03584D22": "3592", "03584L23": "3592",
	} {
		if _, v, ok := LibraryVariantByProduct(product); !ok || v.Family != family {
			t.Errorf("variant %s: family %q, want %q (ok=%v)", product, v.Family, family, ok)
		}
	}
	// Drive caps: the 3573 keeps its long-standing 4; a 3584 frame has 12
	// drive positions.
	for display, want := range map[string]int{"TS3100/TS3200 (3573)": 4, "TS3500 (3584)": 12} {
		found := false
		for _, m := range LibraryModels() {
			if m.Display == display {
				found = true
				if m.MaxDrives != want {
					t.Errorf("%s: MaxDrives %d, want %d", display, m.MaxDrives, want)
				}
			}
		}
		if !found {
			t.Errorf("model %q missing from catalog", display)
		}
	}
}
