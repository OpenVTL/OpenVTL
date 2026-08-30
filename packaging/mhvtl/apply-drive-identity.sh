#!/usr/bin/env bash
#
# OpenVTL — patch mhVTL's IBM Ultrium (LTO) drive personality so an FC-attached
# IBM i classifies the drive as a real 3580-005 AND can vary it on inside a media
# library.
#
# Stock mhVTL ult3580_pm.c emits an incomplete/malformed vendor-VPD set vs a real
# IBM LTO-5: its 0xc0 ("component revision") is double-headered and lacks the
# "LTO5_..." string; it omits 0xe0/0xe1 (vendor capability/command-support tables,
# the drive's analog of the changer's 0xd0); and it omits 0x88 (SCSI Ports, which
# advertises the drive's target-port WWN — IBM i needs it to set up the drive's
# path inside the library, else vary-on fails reason 3404/2009). This:
#   * overrides 0xc0 + adds 0xe0/0xe1 with bytes captured from a reference IBM
#     3580-005 unit (payloads only; the VPD framework prepends the 4-byte header),
#   * adds a 0x88 SCSI Ports page built from the drive's device.conf NAA,
#   * fixes the std INQUIRY transport bits (set CmdQue=1, clear the bogus SPI
#     width bits) so IBM i commits a library BACKUP WRITE to the drive — stock
#     mhVTL advertises CmdQue=0 + a parallel-SCSI personality and IBM i refuses
#     to commit the write (logical not-ready). Confirmed by sg_inq diff vs the
#     reference 3580-005 unit.
#   * bumps INQUIRY to SPC-4.
#
# Idempotent (each piece is checked independently, so it can upgrade a checkout
# that was patched by an earlier version of this script). Run on the LTO-5
# (init_ult3580_td5) personality before `make -C usr`.
#
# Usage:  ./apply-drive-identity.sh /path/to/mhvtl-checkout
#
set -euo pipefail

SRC="${1:?Usage: apply-drive-identity.sh /path/to/mhvtl-checkout}"
PM="$SRC/usr/pm/ult3580_pm.c"
[[ -f "$PM" ]] || { echo "Not found: $PM" >&2; exit 1; }

python3 - "$PM" <<'PYEOF'
import re, sys
path = sys.argv[1]
s = open(path).read()
changed = False

# memcpy()/sscanf() below need <string.h>; ult3580_pm.c doesn't include it.
if '#include <string.h>' not in s:
    s = s.replace('#include "mhvtl_log.h"',
                  '#include "mhvtl_log.h"\n#include <string.h>', 1)
    changed = True

# Page bodies captured from a reference IBM 3580-005 LTO-5 unit (header stripped).
arrays = r'''
/* OpenVTL: real IBM LTO-5 vendor VPD page bodies (field capture, 3580-005). */
static const uint8_t ovtl_c0[] = {  /* 0xc0 component revision: "LTO5_H991 ..." */
	0x4c,0x54,0x4f,0x35,0x5f,0x48,0x39,0x39,0x31,0x20,0x20,0x20,0x32,0x30,0x33,0x30,
	0x33,0x32,0x00,0x32,0x30,0x31,0x37,0x30,0x35,0x30,0x31,0x66,0x63,0x70,0x5f,0x68,
	0x6c,0x5f,0x20,0x20,0x20,0x20,0x20 };
static const uint8_t ovtl_e0[] = {  /* 0xe0 vendor capability table */
	0x04,0x53,0x43,0x44,0x44,0x00,0x01,0x4e,0xb2,0x10,0x00,0x00,0x00,0x0c,0x35,0x00,
	0x00,0x78,0x00,0x00,0x00,0xff,0xff,0xff,0x55,0x4c,0x54,0x52,0x49,0x55,0x4d,0x34,
	0x20,0x20,0xb2,0x10,0x00,0x00,0x00,0x16,0xe3,0x60,0x00,0x8c,0x00,0x00,0x00,0xff,
	0xff,0xff,0x55,0x4c,0x54,0x52,0x49,0x55,0x4d,0x35,0x20,0x20,0x82,0x50,0x00,0x00,
	0x00,0x06,0x1a,0x80,0x00,0x50,0x00,0x00,0x00,0xff,0xff,0xff,0x55,0x4c,0x54,0x52,
	0x49,0x55,0x4d,0x33,0x20,0x20,0x00,0x02,0x0c,0x33,0x35,0x4c,0x32,0x30,0x38,0x36,
	0x20,0x20,0x20,0x20,0x20,0x00,0x03,0x10,0x20,0x00,0x00,0x00,0x20,0x20,0x20,0x20,
	0x20,0x20,0x20,0x20,0x20,0x20,0x20,0x20,0x00,0x04,0x04,0x20,0x00,0x00,0x00,0x00,
	0x06,0x04,0x34,0xc0,0x00,0x00,0x00,0x08,0x10,0x80,0x00,0x00,0x06,0x00,0x00,0x01,
	0x06,0x00,0x00,0x08,0x04,0x00,0x00,0x05,0x05,0x00,0x0b,0x08,0x80,0x00,0x00,0x00,
	0x35,0x80,0x00,0x05,0x00,0x0d,0x03,0x01,0x00,0x00,0x00,0x0e,0x02,0x02,0xd0,0x00,
	0x0f,0x01,0xff,0x00,0x10,0x02,0x02,0xd0,0x00,0x11,0x04,0x82,0x82,0x00,0x17,0x00,
	0x14,0x1e,0x00,0x01,0x01,0x0d,0x04,0x21,0x08,0x20,0x0a,0x20,0x0b,0x10,0x10,0x1d,
	0x11,0xff,0x13,0x20,0x19,0xff,0x1b,0x11,0x1d,0x34,0x2b,0xff,0x3b,0x0c,0x3c,0x0b };
static const uint8_t ovtl_e1[] = {  /* 0xe1 vendor capability table */
	0x04,0x53,0x43,0x44,0x44,0x00,0x18,0x02,0x00,0x78,0x00,0x1b,0x01,0x60,0x00,0x1d,
	0x08,0x00,0x40,0x00,0x00,0x00,0x15,0x08,0x08,0x00,0x20,0x05,0x02,0x46,0x58,0x01,
	0x44,0x00,0x22,0x01,0x08,0x00,0x25,0x10,0x48,0x00,0x80,0x00,0x4c,0x80,0x80,0x00,
	0x58,0x00,0x40,0x00,0x5c,0x80,0x40,0x00,0x20,0x04,0x08,0x00,0x10,0x1d,0x00,0x00,
	0x02,0x01,0x01 };

'''

override = r'''
	/* OpenVTL identity: replay a real IBM LTO-5's vendor VPD pages (3580-005). */
	if (lu->lu_vpd[PCODE_OFFSET(0xc0)])
		dealloc_vpd(lu->lu_vpd[PCODE_OFFSET(0xc0)]);
	lu->lu_vpd[PCODE_OFFSET(0xc0)] = alloc_vpd(sizeof(ovtl_c0));
	if (lu->lu_vpd[PCODE_OFFSET(0xc0)])
		memcpy(lu->lu_vpd[PCODE_OFFSET(0xc0)]->data, ovtl_c0, sizeof(ovtl_c0));
	lu->lu_vpd[PCODE_OFFSET(0xe0)] = alloc_vpd(sizeof(ovtl_e0));
	if (lu->lu_vpd[PCODE_OFFSET(0xe0)])
		memcpy(lu->lu_vpd[PCODE_OFFSET(0xe0)]->data, ovtl_e0, sizeof(ovtl_e0));
	lu->lu_vpd[PCODE_OFFSET(0xe1)] = alloc_vpd(sizeof(ovtl_e1));
	if (lu->lu_vpd[PCODE_OFFSET(0xe1)])
		memcpy(lu->lu_vpd[PCODE_OFFSET(0xe1)]->data, ovtl_e1, sizeof(ovtl_e1));
'''

# 0x88 SCSI Ports page: advertise the drive's target-port WWN from device.conf NAA.
block88 = r'''
	/* OpenVTL 0x88: SCSI Ports page (drive target-port WWN). IBM i needs this to
	 * build the drive's path inside the library; stock mhVTL omits it. */
	if (lu->naa) {
		uint8_t naa88[8] = {0};
		uint8_t *d88;
		if (lu->lu_vpd[PCODE_OFFSET(0x88)])
			dealloc_vpd(lu->lu_vpd[PCODE_OFFSET(0x88)]);
		lu->lu_vpd[PCODE_OFFSET(0x88)] = alloc_vpd(0x18);
		if (lu->lu_vpd[PCODE_OFFSET(0x88)]) {
			d88 = lu->lu_vpd[PCODE_OFFSET(0x88)]->data;
			d88[3]  = 0x01; /* relative port 1 */
			d88[11] = 0x0c; /* target port descriptors length */
			d88[12] = 0x01; /* protocol FCP, code set binary */
			d88[13] = 0x93; /* PIV=1, assoc=target port, designator type 3 (NAA) */
			d88[15] = 0x08; /* designator length */
			sscanf((const char *)lu->naa,
			    "%hhx:%hhx:%hhx:%hhx:%hhx:%hhx:%hhx:%hhx",
			    &naa88[0], &naa88[1], &naa88[2], &naa88[3],
			    &naa88[4], &naa88[5], &naa88[6], &naa88[7]);
			naa88[0] = (naa88[0] & 0x0f) | 0x50; /* NAA type 5 */
			memcpy(&d88[16], naa88, 8);
		}
	}
'''

block_inq = r'''
	/* OpenVTL transport identity: fix the standard INQUIRY to a real FC LTO-5's.
	 * Stock mhVTL (init_lu in vtltape.c) advertises a parallel-SCSI personality
	 * with NO command queuing: byte6=0x01 (Addr16), byte7=0x20 (WBus16), CmdQue=0.
	 * A real FC LTO-5 — the kind IBM i commits library backup writes to — clears
	 * the SPI width bits and sets CmdQue=1: byte6=0x00, byte7=0x02 (Protect is
	 * already set by init_ult_inquiry and matches the field capture).
	 * IBM i reads this at drive-open qualify; without CmdQue it declines to commit
	 * the BACKUP WRITE to a library tape (logical not-ready: no WRITE, no PR).
	 * Confirmed empirically: sg_inq diff vs a live production 3580-005 target. */
	lu->inquiry[6] = 0x00;	/* clear bogus Addr16 (parallel-SCSI width) */
	lu->inquiry[7] = 0x02;	/* CmdQue=1, clear WBus16 */
'''

block_ctl = r'''
	/* OpenVTL: Control mode page (0x0a) — TST=001, i.e. the drive maintains a
	 * SEPARATE task set per I_T nexus (a real multi-initiator / shared
	 * library-drive trait). IBM i reads MODE SENSE 0x0a during drive-open
	 * qualify; stock mhVTL reports TST=000 (shared). Field capture of a live
	 * production 3580-005 target: byte2=0x20 (TST=001), byte4=0x40. Paired
	 * with INQUIRY CmdQue=1 above. */
	{
		struct mode *ovtl_ctl = lookup_mode_pg(&lu->mode_pg, 0x0a, 0);
		if (ovtl_ctl && ovtl_ctl->pcodePointer) {
			ovtl_ctl->pcodePointer[2] = 0x20; /* TST=001 separate task sets */
			ovtl_ctl->pcodePointer[4] = 0x40; /* match field-captured Control byte4 */
		}
	}
'''

def td5_span(text):
    m = re.search(r'void init_ult3580_td5\(struct lu_phy_attr \*lu\) \{', text)
    if not m:
        sys.exit("[x] init_ult3580_td5 not found in %s" % path)
    return m.start(), text.index('\n}\n', m.end()) + 3

# --- main patch: SPC-4 + c0 override + e0/e1 ---
if 'ovtl_e0' not in s:
    a, b = td5_span(s)
    func = s[a:b]
    func, n = re.subn(r'(drive_ANSI_VERSION\s*=\s*)5;', r'\g<1>6;', func, count=1)
    if n != 1:
        sys.exit("[x] drive_ANSI_VERSION = 5 not found in init_ult3580_td5")
    if 'init_ult_inquiry(lu);' not in func:
        sys.exit("[x] init_ult_inquiry(lu); not found in init_ult3580_td5")
    func = func.replace('init_ult_inquiry(lu);', 'init_ult_inquiry(lu);\n' + override, 1)
    s = s[:a] + arrays + func + s[b:]
    changed = True

# --- 0x88 page (independent; upgrades a checkout already patched for c0/e0/e1) ---
if 'OpenVTL 0x88' not in s:
    a, b = td5_span(s)
    func = s[a:b]
    anchor = 'memcpy(lu->lu_vpd[PCODE_OFFSET(0xe1)]->data, ovtl_e1, sizeof(ovtl_e1));'
    if anchor not in func:
        sys.exit("[x] 0xe1 override not found — run the main drive patch first")
    func = func.replace(anchor, anchor + '\n' + block88, 1)
    s = s[:a] + func + s[b:]
    changed = True

# --- transport-bit fix (independent): std INQUIRY CmdQue/SPI -> match the
#     reference capture so IBM i commits a library BACKUP WRITE to the drive. -
if 'OpenVTL transport identity' not in s:
    a, b = td5_span(s)
    func = s[a:b]
    anchor = 'init_ult_inquiry(lu);'
    if anchor not in func:
        sys.exit("[x] init_ult_inquiry(lu); not found — cannot place transport-bit fix")
    func = func.replace(anchor, anchor + '\n' + block_inq, 1)
    s = s[:a] + func + s[b:]
    changed = True

# --- Control-page TST=001 (independent): field-captured multi-initiator task sets.
#     Guard keys on the injected CODE identifier (ovtl_ctl), not the comment text:
#     it must match trees patched by every prior version of this block. ---
if 'ovtl_ctl' not in s:
    a, b = td5_span(s)
    func = s[a:b]
    anchor = 'add_mode_control(lu);'
    if anchor not in func:
        sys.exit("[x] add_mode_control(lu); not found — cannot place Control TST fix")
    func = func.replace(anchor, anchor + '\n' + block_ctl, 1)
    s = s[:a] + func + s[b:]
    changed = True

if changed:
    open(path, 'w').write(s)
    print("[+] drive-identity patch applied/updated (SPC-4 + 0xc0 + 0xe0/0xe1 + 0x88 + INQUIRY CmdQue/SPI + Control TST=001).")
else:
    print("[=] drive-identity already fully patched — nothing to do.")
PYEOF

# Verify
if grep -q 'ovtl_e0' "$PM" && grep -qE 'drive_ANSI_VERSION[[:space:]]*=[[:space:]]*6;' "$PM" \
   && grep -q 'OpenVTL 0x88' "$PM" && grep -q 'OpenVTL transport identity' "$PM" \
   && grep -q 'ovtl_ctl' "$PM"; then
  echo "[+] drive-identity patch verified (c0/e0/e1 + SPC-4 + 0x88 + INQUIRY CmdQue/SPI + Control TST=001)."
else
  echo "[x] drive-identity patch verification failed — inspect $PM." >&2
  exit 1
fi
