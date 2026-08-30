package inventory

// Model catalog — what the installed mhVTL 1.8.0 emulates, verified
// against the installed binaries (strings extraction of vtllibrary/
// vtltape personality tables + mktape's density list). The wizard
// only creates IBMi-compatible entries;
// the rest are listed for honesty and greyed in the UI.
//
// mktape on this build has no LTO9 density, so the TD9 drive is
// offered with LTO8 media (an LTO-9 drive writes gen-8 media). TD1/TD2
// and the half-height variants exist in vtltape but are omitted — no
// modern IBM i attaches them and they'd only pad the dropdown.

type DriveModel struct {
	Product    string `json:"product"` // device.conf "Product identification"
	Vendor     string `json:"vendor"`
	Display    string `json:"display"`
	Family     string `json:"family"`      // LTO | 3592 | T10K
	Density    string `json:"density"`     // mktape -d value for its media
	Suffix     string `json:"suffix"`      // barcode suffix (L5, JA, …)
	CapacityMB int    `json:"capacity_mb"` // native cart capacity (decimal MB); mint uses it, no user size
	IBMi       bool   `json:"ibmi_compatible"`
	Note       string `json:"note,omitempty"`
}

type LibraryVariant struct {
	Product   string `json:"product"` // device.conf "Product identification"
	Family    string `json:"family"`  // drive family this frame accepts
	Display   string `json:"display"`
	Creatable bool   `json:"creatable"` // wizard may create THIS variant
}

type LibraryModel struct {
	Vendor    string           `json:"vendor"`
	Display   string           `json:"display"`
	Variants  []LibraryVariant `json:"variants"`
	IBMi      bool             `json:"ibmi_compatible"`
	Creatable bool             `json:"creatable"`  // some variant is creatable
	MaxDrives int              `json:"max_drives"` // per-library drive cap (real frame limit)
	Note      string           `json:"note,omitempty"`
}

// LTO capacities are the standard native (uncompressed) sizes, in decimal
// MB (1 GB = 1000 MB). The virtual carts are sparse, so this is the max
// size cap, not upfront disk use. TD9 mints L8 media but is an LTO-9 drive,
// so it takes the LTO-9 capacity per the drive's generation. 3592/T10K use
// their native generation capacities.
var driveModels = []DriveModel{
	{Product: "ULT3580-TD3", Vendor: "IBM", Display: "IBM LTO-3 (ULT3580-TD3)", Family: "LTO", Density: "LTO3", Suffix: "L3", CapacityMB: 400_000, IBMi: true},
	{Product: "ULT3580-TD4", Vendor: "IBM", Display: "IBM LTO-4 (ULT3580-TD4)", Family: "LTO", Density: "LTO4", Suffix: "L4", CapacityMB: 800_000, IBMi: true},
	{Product: "ULT3580-TD5", Vendor: "IBM", Display: "IBM LTO-5 (ULT3580-TD5)", Family: "LTO", Density: "LTO5", Suffix: "L5", CapacityMB: 1_500_000, IBMi: true,
		Note: "validated against IBM i"},
	{Product: "ULT3580-TD6", Vendor: "IBM", Display: "IBM LTO-6 (ULT3580-TD6)", Family: "LTO", Density: "LTO6", Suffix: "L6", CapacityMB: 2_500_000, IBMi: true},
	{Product: "ULT3580-TD7", Vendor: "IBM", Display: "IBM LTO-7 (ULT3580-TD7)", Family: "LTO", Density: "LTO7", Suffix: "L7", CapacityMB: 6_000_000, IBMi: true},
	{Product: "ULT3580-TD8", Vendor: "IBM", Display: "IBM LTO-8 (ULT3580-TD8)", Family: "LTO", Density: "LTO8", Suffix: "L8", CapacityMB: 12_000_000, IBMi: true},
	{Product: "ULT3580-TD9", Vendor: "IBM", Display: "IBM LTO-9 (ULT3580-TD9)", Family: "LTO", Density: "LTO8", Suffix: "L8", CapacityMB: 18_000_000, IBMi: true,
		Note: "installed mktape has no LTO9 media type — carts are minted as L8, which an LTO-9 drive writes"},
	{Product: "03592J1A", Vendor: "IBM", Display: "IBM 3592 J1A", Family: "3592", Density: "J1A", Suffix: "JA", CapacityMB: 300_000, IBMi: true},
	{Product: "03592E05", Vendor: "IBM", Display: "IBM 3592 E05 (TS1120)", Family: "3592", Density: "E05", Suffix: "JA", CapacityMB: 500_000, IBMi: true},
	{Product: "03592E06", Vendor: "IBM", Display: "IBM 3592 E06 (TS1130)", Family: "3592", Density: "E06", Suffix: "JB", CapacityMB: 1_000_000, IBMi: true},
	{Product: "03592E07", Vendor: "IBM", Display: "IBM 3592 E07 (TS1140)", Family: "3592", Density: "E07", Suffix: "JB", CapacityMB: 4_000_000, IBMi: true},
	{Product: "T10000B", Vendor: "STK", Display: "STK T10000B", Family: "T10K", Density: "T10KB", Suffix: "T1", CapacityMB: 1_000_000, IBMi: false,
		Note: "IBM i does not attach STK drives"},
}

// ibm03584Variants: TS3500 frames. Families per the TS3500 operator
// manual: "LTO frames (x32, x52, x53) and 3592 frames (x22, x23)". The earlier catalog had L22/D22 as LTO and
// L53/D53 as 3592 — both backwards — and listed a DLT-era L42 as LTO
// (dropped: IBM i never attaches DLT). vtllibrary routes any 03584*
// product id to its 3584 personality, so every string here is servable.
//
// Creatable set: 03584L32 only — the exact model the identity work was
// validated against (GA32-0454-00 + mhVTL patches 11-15,
// wire-verified against a live IBM i attach). The 03584403 iSCSI VTL
// variant was retired 2026-08-24 — the product is FC-only (see
// docs/why-fc-only.md). mhVTL patch 16, its data-plane personality,
// stays applied but dormant. The D-frames are
// expansion frames with no accessor/control path — real ones never
// present a standalone changer LUN, so they stay listed for honesty
// but non-creatable.
var ibm03584Variants = []LibraryVariant{
	{Product: "03584L32", Family: "LTO", Display: "3584-L32 (LTO base frame)", Creatable: true},
	{Product: "03584L52", Family: "LTO", Display: "3584-L52 (LTO base frame)"},
	{Product: "03584L53", Family: "LTO", Display: "3584-L53 (LTO base frame)"},
	{Product: "03584D32", Family: "LTO", Display: "3584-D32 (LTO expansion frame)"},
	{Product: "03584D52", Family: "LTO", Display: "3584-D52 (LTO expansion frame)"},
	{Product: "03584D53", Family: "LTO", Display: "3584-D53 (LTO expansion frame)"},
	{Product: "03584L22", Family: "3592", Display: "3584-L22 (3592 base frame)"},
	{Product: "03584L23", Family: "3592", Display: "3584-L23 (3592 base frame)"},
	{Product: "03584D22", Family: "3592", Display: "3584-D22 (3592 expansion frame)"},
	{Product: "03584D23", Family: "3592", Display: "3584-D23 (3592 expansion frame)"},
}

var libraryModels = []LibraryModel{
	{
		// Display convention (operator decision 2026-09-01): marketing name
		// + bare type in parens, matching what WRKHDWRSC shows host-side
		// (type-model 3573-040 / 3584-040) — never the SCSI product id,
		// which stays a wire/technical identifier.
		Vendor: "IBM", Display: "TS3100/TS3200 (3573)",
		Variants:  []LibraryVariant{{Product: "3573-TL", Family: "LTO", Display: "3573-TL", Creatable: true}},
		IBMi:      true,
		Creatable: true,
		MaxDrives: 4, // TS3200 tops out at 4 half-height; the long-standing cap here
		Note:      "the library model proven against the IBM i on this project",
	},
	{
		Vendor: "IBM", Display: "TS3500 (3584)",
		Variants:  ibm03584Variants,
		IBMi:      true,
		Creatable: true, // L32 only — see ibm03584Variants
		MaxDrives: 12,   // 12 drive positions per real frame
		Note:      "L32 field-validated against the IBM i 2026-07-18 (vary on + SAVLIB over FC)",
	},
	{
		Vendor: "STK", Display: "STK L20/L40/L80 · L-series · SL-series",
		IBMi: false, Creatable: false,
		Note: "emulated by mhVTL but not attachable by IBM i",
	},
	{
		Vendor: "HP", Display: "HP EML E-series · MSL series",
		IBMi: false, Creatable: false,
		Note: "emulated by mhVTL but not attachable by IBM i",
	},
	{
		Vendor: "Spectra", Display: "Spectra Treefrog · Gator · T-series",
		IBMi: false, Creatable: false,
		Note: "emulated by mhVTL but not attachable by IBM i",
	},
	{
		Vendor: "Quantum", Display: "Scalar series · Overland NEO",
		IBMi: false, Creatable: false,
		Note: "emulated by mhVTL but not attachable by IBM i",
	},
}

func LibraryModels() []LibraryModel { return libraryModels }
func DriveModels() []DriveModel     { return driveModels }

// DriveModelByProduct resolves a device.conf product id to its catalog
// entry (mint uses it for density + barcode suffix).
func DriveModelByProduct(product string) (DriveModel, bool) {
	for _, d := range driveModels {
		if d.Product == product {
			return d, true
		}
	}
	return DriveModel{}, false
}

// LibraryVariantByProduct resolves a library product id (e.g.
// "03584L32") to its variant + parent model.
func LibraryVariantByProduct(product string) (LibraryModel, LibraryVariant, bool) {
	for _, m := range libraryModels {
		for _, v := range m.Variants {
			if v.Product == product {
				return m, v, true
			}
		}
	}
	return LibraryModel{}, LibraryVariant{}, false
}

// CompatibleDrives lists catalog drives a library variant accepts —
// same family, and only IBMi-compatible ones for IBMi libraries.
func CompatibleDrives(v LibraryVariant) []DriveModel {
	var out []DriveModel
	for _, d := range driveModels {
		if d.Family == v.Family {
			out = append(out, d)
		}
	}
	return out
}
