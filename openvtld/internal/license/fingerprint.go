// Package license derives the appliance's support key: a stable, human-
// readable fingerprint of the machine's durable identity that support uses
// to associate an instance with a paid account.
//
// The key is DERIVED on demand, never stored as authoritative — there is
// nothing for the user to edit, and changing it means changing the machine.
// Inputs are chosen to survive field repairs and routine ops but to re-key
// on a genuine machine/OS change:
//
//   - /etc/machine-id       — the systemd install identity (new on OS reinstall)
//   - board/system identity — DMI product_uuid (x86) or device-tree system-id
//     (POWER); board-level, so replacing a failed HBA/NIC/disk does NOT re-key.
//
// Deliberately excluded: anything field-replaceable (HBA WWN, NIC MAC, disk
// serials), storage GUIDs (a pool rebuild is a routine op), hostname/IP.
package license

import (
	"crypto/sha256"
	"os"
	"strings"
)

// Crockford base32 alphabet — omits I L O U so the key is unambiguous when
// read aloud to support.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Compute derives this appliance's support key. It fails only if the machine
// identity can't be read at all (no /etc/machine-id); a missing board id
// degrades to machine-id alone rather than failing.
func Compute() (string, error) {
	machineID, err := readTrim("/etc/machine-id")
	if err != nil {
		return "", err
	}
	return derive(machineID, boardID()), nil
}

// derive is the pure fingerprint function — testable without host files.
func derive(machineID, board string) string {
	h := sha256.New()
	h.Write([]byte("openvtl-license-v1\x00")) // domain separation
	h.Write([]byte(machineID))
	h.Write([]byte{0})
	h.Write([]byte(board))
	return format(h.Sum(nil))
}

// boardID returns a board/system-level identity. x86 exposes the SMBIOS
// system UUID; POWER has no DMI, so the device tree's system-id is used.
func boardID() string {
	if v, err := readTrim("/sys/class/dmi/id/product_uuid"); err == nil && v != "" {
		return "dmi:" + v
	}
	if v, err := readTrim("/proc/device-tree/system-id"); err == nil && v != "" {
		return "dt:" + strings.Trim(v, "\x00")
	}
	return "" // degrade to machine-id only
}

func readTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// format renders OVTL-XXXXX-XXXXX-XXXXX-XXXXX: 19 Crockford-base32 data
// symbols from the hash plus a position-weighted checksum symbol (catches
// single-symbol typos and transpositions when the key is read to support).
func format(sum []byte) string {
	data := make([]byte, 0, 20)
	weighted := 0
	for i := 0; i < 19; i++ {
		idx := int(sum[i]) & 31
		data = append(data, crockford[idx])
		weighted += (i + 1) * idx
	}
	data = append(data, crockford[weighted%32])

	var sb strings.Builder
	sb.WriteString("OVTL")
	for i := 0; i < 20; i += 5 {
		sb.WriteByte('-')
		sb.Write(data[i : i+5])
	}
	return sb.String()
}

// Valid reports whether key has the OVTL format and a correct checksum —
// support tooling can use it to catch a mistyped key before a lookup.
func Valid(key string) bool {
	if len(key) != 28 || !strings.HasPrefix(key, "OVTL-") {
		return false
	}
	body := make([]byte, 0, 20)
	for i := 4; i < len(key); i++ {
		c := key[i]
		if i == 4 || i == 10 || i == 16 || i == 22 {
			if c != '-' {
				return false
			}
			continue
		}
		if strings.IndexByte(crockford, c) < 0 {
			return false
		}
		body = append(body, c)
	}
	if len(body) != 20 {
		return false
	}
	weighted := 0
	for i := 0; i < 19; i++ {
		weighted += (i + 1) * strings.IndexByte(crockford, body[i])
	}
	return crockford[weighted%32] == body[19]
}
