package inventory

import "testing"

// The 3584 changer presents its serial per GA32-0454-00 (mhVTL patches
// 12/13): INQUIRY 38-49 right-justified zero-padded, VPD 0x80 payload
// with the 4-hex First Storage Element Address appended — sg_vpd reads
// the whole payload. SerialMatches must accept those forms and nothing
// looser.
func TestSerialMatches(t *testing.T) {
	cases := []struct {
		dev, conf string
		want      bool
	}{
		{"OVTD832766", "OVTD832766", true},       // drives: exact
		{"00OVTL409366", "OVTL409366", true},     // 3584 INQUIRY presentation
		{"00OVTL4093660400", "OVTL409366", true}, // 3584 VPD 0x80 (addr suffix)
		{"OVTL409366", "OVTL409366", true},       // 3573 changer: exact
		{"00OVTL999999", "OVTL409366", false},    // different serial, padded
		{"00OVTL40936604", "OVTL409366", false},  // wrong suffix length
		{"OVTL4093660400", "OVTL409367", false},  // suffix form, wrong serial
		{"", "OVTL409366", false},
		{"OVTL409366", "", false}, // never match an empty conf serial
	}
	for _, c := range cases {
		if got := SerialMatches(c.dev, c.conf); got != c.want {
			t.Errorf("SerialMatches(%q, %q) = %v, want %v", c.dev, c.conf, got, c.want)
		}
	}
}
