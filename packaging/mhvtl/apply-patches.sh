#!/usr/bin/env bash
#
# OpenVTL — apply required source patches to an mhVTL checkout before building
# the userspace. Run AFTER cloning mhVTL and BEFORE `make -C usr`.
#
# Patch 1 — element addresses for the IBM 3573-TL (TS3100) personality.
#   mhVTL 1.8's 3573-TL profile in usr/pm/ibm_smc_pm.c defaults to IE start
#   0x0010 (16) and storage start 0x1000 (4096). Existing BRMS enrollments use
#   IE start 768 and slot start 1024 (drive start 256 already matches).
#   Shifting these avoids re-enrollment. Verified via raw READ ELEMENT STATUS.
#
# Patch 2 — VPD page 0xd0 single-header fix (changer identity for IBM i 3573-020).
#   update_ibm_3100_vpd_d0()'s hardcoded pg_d0[] array embeds its OWN page header
#   (08 d0 00 c0 ...) AND the VPD framework adds a second header, so the changer
#   emits a malformed double header (08 d0 00 c8 [08 d0 00 c0 ...]). A real TS3100
#   — the identity IBM i classifies as 3573-020 — emits a clean single header
#   (08 d0 00 c0 ...). We skip the embedded 4-byte header and size the page to its
#   192-byte payload so the framework header is the only one.
#
# Patch 3 — MODE SENSE "Saved values" (Page Control = 3) in usr/spc.c.
#   Stock mhVTL rejects PC=3 with ILLEGAL REQUEST / SAVING PARAMETERS NOT SUPPORTED
#   (sense 05 39 00). A real IBM LTO drive answers Saved-page requests, and IBM i
#   issues MODE SENSE PC=3 while varying on a tape drive inside a media library —
#   the rejection makes the drive vary-on fail (reason 3404). We report Current
#   values for Saved (set pc=0) so vary-on succeeds.
#
# Each patch is content-matched (not line-numbered) and idempotent — re-running
# after a successful patch is a no-op.
#
# Usage:  ./apply-patches.sh /path/to/mhvtl-checkout
#
set -euo pipefail

SRC="${1:?Usage: apply-patches.sh /path/to/mhvtl-checkout}"
PM="$SRC/usr/pm/ibm_smc_pm.c"
[[ -f "$PM" ]] || { echo "Not found: $PM" >&2; exit 1; }

# --- Patch 1: 3573-TL element addresses (IE -> 0x0300, slots -> 0x0400) --------
if grep -qE 'start_(map|storage)[[:space:]]*=[[:space:]]*0x(0010|1000);' "$PM"; then
  sed -i -E 's/(start_map[[:space:]]*=[[:space:]]*)0x0010;/\10x0300;/'   "$PM"
  sed -i -E 's/(start_storage[[:space:]]*=[[:space:]]*)0x1000;/\10x0400;/' "$PM"
  if grep -qE 'start_map[[:space:]]*=[[:space:]]*0x0300;' "$PM" \
     && grep -qE 'start_storage[[:space:]]*=[[:space:]]*0x0400;' "$PM"; then
    echo "[+] Patch 1: 3573-TL element addresses -> IE 0x0300 (768), slots 0x0400 (1024); drive unchanged 0x0100 (256)."
  else
    echo "[x] Patch 1 verification failed — inspect $PM manually." >&2
    exit 1
  fi
else
  echo "[=] Patch 1: element addresses already patched (or profile changed) — nothing to do."
fi

# --- Patch 2: VPD 0xd0 single-header (so IBM i classifies the changer as 3573) -
#   Guard requires the UNPATCHED alloc_vpd(0xc8) as well as the memcpy: the
#   memcpy string alone re-fired on trees where Patch 15's generated 3584
#   function used the same form, corrupting it (field lesson — patch 15 now
#   emits `memcpy(d, pg_d0, ...)` so neither the grep nor the sed can ever
#   touch it; this guard is the second belt).
if grep -qF 'memcpy(d, &pg_d0[0], sizeof(pg_d0));' "$PM" && grep -qF 'alloc_vpd(0xc8)' "$PM"; then
  sed -i \
    -e 's/alloc_vpd(0xc8)/alloc_vpd(0xc0)/' \
    -e 's/memcpy(d, &pg_d0\[0\], sizeof(pg_d0));/memcpy(d, \&pg_d0[4], 0xc0);/' \
    "$PM"
  if grep -qF 'memcpy(d, &pg_d0[4], 0xc0);' "$PM" && grep -qF 'alloc_vpd(0xc0)' "$PM"; then
    echo "[+] Patch 2: VPD 0xd0 now emits a single-header page (08 d0 00 c0 ...)."
  else
    echo "[x] Patch 2 verification failed — inspect update_ibm_3100_vpd_d0() in $PM." >&2
    exit 1
  fi
else
  echo "[=] Patch 2: VPD 0xd0 already single-header (or source changed) — nothing to do."
fi

# --- Patch 3: MODE SENSE Saved values (PC=3) -> report Current (drive vary-on) -
SPC="$SRC/usr/spc.c"
[[ -f "$SPC" ]] || { echo "Not found: $SPC" >&2; exit 1; }
python3 - "$SPC" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL: Saved mode pages' in s:
    print("[=] Patch 3: MODE SENSE Saved-values already handled — nothing to do.")
    sys.exit(0)
# Replace the whole `if (0x3 == pc) { ... Reporting on Saved Values ... }` block.
pat = re.compile(r'if \(0x3 == pc\)\s*\{[^{}]*?Reporting on Saved Values not supported[^{}]*?\}', re.S)
new = ('if (0x3 == pc) /* OpenVTL: Saved mode pages -> report Current values '
       '(real IBM LTO drives support Saved; IBM i reads them at drive vary-on) */\n'
       '\t\tpc = 0;')
s2, n = pat.subn(new, s, count=1)
if n == 1:
    open(p, 'w').write(s2)
    print("[+] Patch 3: MODE SENSE Saved values (PC=3) now reports Current values.")
else:
    sys.stderr.write("[x] Patch 3: spc_mode_sense() PC==3 block not found — inspect usr/spc.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 4: PR REPORT CAPABILITIES — CRH=0 + full type mask ------------------
#   resp_spc_pri() service action 2 hardcodes buf[2]=0x10 => CRH=1 (Compatible
#   Reservation Handling). With CRH=1 the IBM i takes the legacy SPC-2 RESERVE
#   (0x16) path and, for a media-library tape drive, never escalates to PERSISTENT
#   RESERVE — it qualifies the drive, then refuses to commit the BACKUP WRITE
#   ("device no longer in ready status", nothing logged to the PAL). A reference
#   production VTL capture (3580-005 identity, one IBM i commits library writes
#   to) reports CRH=0 (buf[2]=0x00) + the full PR type mask, which forces the
#   IBM i onto PERSISTENT RESERVE (type 3, Exclusive Access) — the path it
#   commits the write on. Match that capture exactly: CRH=0, TMV=1 (buf[3]=0x80
#   unchanged), type mask buf[4]=0xea + buf[5]=0x01 (all 6 reservation types).
#   Confirmed by sg_persist --in --report-capabilities diff 2026-06-28.
python3 - "$SPC" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL: PR capabilities' in s:
    print("[=] Patch 4: PR REPORT CAPABILITIES already matched — nothing to do.")
    sys.exit(0)
# Match the REPORT CAPABILITIES body and rewrite the capability bytes.
pat = re.compile(
    r'(case 2: /\* REPORT CAPABILITIES \*/\s*\n)'
    r'\s*buf\[1\]\s*=\s*8;\s*\n'
    r'\s*buf\[2\]\s*=\s*0x10;\s*\n'
    r'\s*buf\[3\]\s*=\s*0x80;\s*\n'
    r'\s*buf\[4\]\s*=\s*0x08;\s*\n', re.S)
new = (r'\1'
       '\t\tbuf[1]    = 8;\n'
       '\t\tbuf[2]    = 0x00; /* OpenVTL: PR capabilities - CRH=0 (force IBM i to PERSISTENT RESERVE; field-validated) */\n'
       '\t\tbuf[3]    = 0x80; /* TMV=1 */\n'
       '\t\tbuf[4]    = 0xea; /* type mask: WR_EX|EX_AC|WR_EX_RO|EX_AC_RO|WR_EX_AR */\n'
       '\t\tbuf[5]    = 0x01; /* type mask: EX_AC_AR */\n')
s2, n = pat.subn(new, s, count=1)
if n == 1:
    open(p, 'w').write(s2)
    print("[+] Patch 4: PR REPORT CAPABILITIES now CRH=0 + full type mask (match the reference capture).")
else:
    sys.stderr.write("[x] Patch 4: resp_spc_pri() REPORT CAPABILITIES block not found — inspect usr/spc.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 5: PR IN READ FULL STATUS (service action 3) in resp_spc_pri() ------
#   Stock mhVTL implements PR IN service actions 0 (READ KEYS), 1 (READ
#   RESERVATION) and 2 (REPORT CAPABILITIES) but lumps SA 3 (READ FULL STATUS)
#   into default: -> ILLEGAL REQUEST / INVALID FIELD IN CDB (sense 05 24 00).
#   Once Patch 4 makes the IBM i use PERSISTENT RESERVE (CRH=0), it walks the PR
#   negotiation REPORT CAPABILITIES -> READ KEYS -> *READ FULL STATUS* before it
#   registers; the SA 3 rejection aborts the whole thing and the library save fails
#   "device no longer in ready status". A real IBM LTO answers READ FULL STATUS,
#   and the reference production VTL does too. Implement it:
#   PRGENERATION + (no registrants -> zero additional length; one registrant -> a
#   24-byte full-status descriptor, no TransportID). Confirmed by mhVTL -d trace
#   2026-06-28 (SA3 -> 05 24 00 right before the IBM i gives up).
python3 - "$SPC" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL: IBM i requires it to use PERSISTENT' in s:
    print("[=] Patch 5: PR IN READ FULL STATUS already implemented — nothing to do.")
    sys.exit(0)
pat = re.compile(r'\tcase 3: /\* READ FULL STATUS \*/\n\tdefault:\n')
impl = (
    '\tcase 3: /* READ FULL STATUS (OpenVTL: IBM i requires it to use PERSISTENT\n'
    '\t\t * RESERVE for a library backup; real IBM LTO drives answer it —\n'
    '\t\t * field-validated) */\n'
    '\t\tput_unaligned_be32(SPR_Reservation_Generation, &buf[0]);\n'
    '\t\tif (!SPR_Reservation_Key) {\n'
    '\t\t\tput_unaligned_be32(0, &buf[4]); /* no registrants */\n'
    '\t\t\tdbuf_p->sz = 8;\n'
    '\t\t\tbreak;\n'
    '\t\t}\n'
    '\t\tput_unaligned_be32(24, &buf[4]); /* one 24-byte descriptor, no TransportID */\n'
    '\t\tput_unaligned_be64(SPR_Reservation_Key, &buf[8]); /* RESERVATION KEY */\n'
    '\t\tif (SPR_Reservation_Type) {\n'
    '\t\t\tbuf[20] = 0x01; /* R_HOLDER (reservation holder) */\n'
    '\t\t\tbuf[21] = SPR_Reservation_Type; /* SCOPE=LU, TYPE */\n'
    '\t\t}\n'
    '\t\tdbuf_p->sz = 32;\n'
    '\t\tbreak;\n'
    '\tdefault:\n')
s2, n = pat.subn(impl, s, count=1)
if n == 1:
    open(p, 'w').write(s2)
    print("[+] Patch 5: PR IN READ FULL STATUS (SA 3) now implemented.")
else:
    sys.stderr.write("[x] Patch 5: resp_spc_pri() 'case 3 / default' block not found — inspect usr/spc.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 6: Tape Capacity log page 0x31 — report real capacity, not 0xfffffffe -
#   update_TapeCapacity() in usr/mhvtl_log.c computes the real capacity into `cap`
#   then THROWS IT AWAY and hardcodes 0xfffffffe in all four fields (the in-tree
#   "fixme" admits it). Over SCSI that reads as an impossible ~4 PB for an LTO-5.
#   The IBM i reads page 0x31 during the SAV capacity-validation sweep and refuses
#   to commit the data write to a tape claiming bogus capacity ("device no longer in
#   ready status") — INZ writes a label without this check, so it succeeds, but SAV
#   does not. Report the real cartridge capacity (mam.max_capacity / capacity_unit,
#   in MiB) instead; partition1 (no alternate partition) = 0. Confirmed by mhVTL -d
#   + sg_logs 2026-06-28 (0x31 = 4278190079 MiB regardless of cart; 0x0c = real 500MB).
LOG="$SRC/usr/mhvtl_log.c"
[[ -f "$LOG" ]] || { echo "Not found: $LOG" >&2; exit 1; }
python3 - "$LOG" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL: report the real cartridge capacity' in s:
    print("[=] Patch 6: Tape Capacity 0x31 already real — nothing to do.")
    sys.exit(0)
pat = re.compile(
    r'\t\tcap = get_unaligned_be64\(&mam\.remaining_capacity\) / lu_ssc\.capacity_unit;\n'
    r'\t\tput_unaligned_be32\(0xfffffffe, &pg->partition0remaining\);\n'
    r'\t\tput_unaligned_be32\(0xfffffffe, &pg->partition1remaining\);\n'
    r'\n'
    r'\t\tcap = get_unaligned_be64\(&mam\.max_capacity\) / lu_ssc\.capacity_unit;\n'
    r'\t\tput_unaligned_be32\(0xfffffffe, &pg->partition0maximum\);\n'
    r'\t\tput_unaligned_be32\(0xfffffffe, &pg->partition1maximum\);\n')
new = (
    '\t\t/* OpenVTL: report the real cartridge capacity (MiB), not the 0xfffffffe\n'
    '\t\t * (~4 PB) stub the IBM i rejects when validating capacity before a SAV. */\n'
    '\t\tcap = get_unaligned_be64(&mam.max_capacity) / lu_ssc.capacity_unit;\n'
    '\t\tput_unaligned_be32(cap, &pg->partition0remaining);\n'
    '\t\tput_unaligned_be32(0, &pg->partition1remaining);\n'
    '\t\tput_unaligned_be32(cap, &pg->partition0maximum);\n'
    '\t\tput_unaligned_be32(0, &pg->partition1maximum);\n')
s2, n = pat.subn(new, s, count=1)
if n == 1:
    open(p, 'w').write(s2)
    print("[+] Patch 6: Tape Capacity 0x31 now reports real cartridge capacity.")
else:
    sys.stderr.write("[x] Patch 6: update_TapeCapacity() 0xfffffffe block not found — inspect usr/mhvtl_log.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 7: call update_TapeCapacity() in ssc_log_sense() (else 0x31 is static) -
#   ssc_log_sense()'s TAPE_CAPACITY case is grouped with DATA_COMPRESSION and only
#   `break;`s — it never calls update_TapeCapacity(), so page 0x31 always returns the
#   struct's static init (0xfffffffe stored host-endian -> 0xFEFFFFFF over SCSI).
#   Patch 6 fixes the updater; this wires it into the read path like the other pages
#   (SEQUENTIAL_ACCESS_DEVICE, VOLUME_STATISTICS, TAPE_USAGE all call their updater).
SSC_C="$SRC/usr/ssc.c"
[[ -f "$SSC_C" ]] || { echo "Not found: $SSC_C" >&2; exit 1; }
python3 - "$SSC_C" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL: serve real capacity' in s:
    print("[=] Patch 7: update_TapeCapacity() already wired into log sense — nothing to do.")
    sys.exit(0)
pat = re.compile(r'\tcase TAPE_CAPACITY:\n\tcase DATA_COMPRESSION:\n\t\tbreak;\n')
new = ('\tcase TAPE_CAPACITY:\n'
       '\t\tupdate_TapeCapacity((struct TapeCapacity_pg *)buf); /* OpenVTL: serve real capacity */\n'
       '\t\tbreak;\n'
       '\n'
       '\tcase DATA_COMPRESSION:\n'
       '\t\tbreak;\n')
s2, n = pat.subn(new, s, count=1)
if n == 1:
    open(p, 'w').write(s2)
    print("[+] Patch 7: update_TapeCapacity() now called from ssc_log_sense().")
else:
    sys.stderr.write("[x] Patch 7: 'case TAPE_CAPACITY/DATA_COMPRESSION' block not found — inspect usr/ssc.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 8: READ POSITION BOP flag — assert ONLY at block 0 (THE labelled-tape gate) -
#   ssc.c's SET_BOP macro asserts BOP (beginning-of-partition) when blk_number < 2,
#   i.e. at block 0 AND block 1. Block 1 is exactly where every VOL1-label read
#   lands, so after the IBM i reads an 80-byte VOL1 it asks READ POSITION and the
#   drive claims BOP while at block 1 — a self-contradicting device. The IBM i
#   rewinds, retries the open, gets the same lie, and fails the operation with
#   CPF4315 "device no longer in ready status" (no PAL entry, no WRITE issued).
#   Blank tapes never reach block 1, which is why ONLY blank-tape INZ ever worked.
#   Spec: IBM LTO SCSI Reference GA32-0928-06 5.2.22.2 (doc p.126): BOP 1b = "the
#   device is at the beginning of the current partition", 0b = "the current
#   logical position is not at the beginning of partition" — strictly block 0.
#   (NB: BYCU=1 in the same byte is CORRECT per the same spec table — IBM reports
#   byte-count-unknown=1; the reference capture's 0 is its own deviation. Leave it.)
#   Confirmed live 2026-07-02: journal shows "Setting BOP" + "Positioned at
#   partition/block 0/1" in every failing SAV/INZ-on-labelled trace; the one
#   working path (INZ on true blank) never leaves block 0.
python3 - "$SSC_C" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 8' in s:
    print("[=] Patch 8: SET_BOP already strict — nothing to do.")
    sys.exit(0)
old = '\t\tif ((blk_number) < 2) {                                   \\'
new = ('\t\tif ((blk_number) == 0) { /* OpenVTL Patch 8: BOP only at block 0, GA32-0928-06 5.2.22.2 */ \\')
if old not in s:
    # tolerate whitespace variations
    pat = re.compile(r'(\n\s*)if \(\(blk_number\) < 2\) \{(\s*\\)')
    s2, n = pat.subn(r'\1if ((blk_number) == 0) { /* OpenVTL Patch 8: BOP only at block 0, GA32-0928-06 5.2.22.2 */\2', s, count=1)
else:
    s2, n = s.replace(old, new, 1), 1
if n == 1 and 'OpenVTL Patch 8' in s2:
    open(p, 'w').write(s2)
    print("[+] Patch 8: READ POSITION BOP now asserted ONLY at block 0.")
else:
    sys.stderr.write("[x] Patch 8: SET_BOP '< 2' condition not found — inspect usr/ssc.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 9: store host data RAW on media — ZFS does compression+dedup below --
#   The cartridge pool lives on ZFS (compression + block-level dedup below).
#   mhVTL's own LZO/zlib would scramble block contents BEFORE ZFS sees them,
#   destroying cross-save dedup. device.conf "Compression: ... enabled 0" only
#   sets the boot default: the IBM i enables drive compaction at open time via
#   MODE SELECT pg 0x0f DCE=1 (COMPACT(*DEV)), which flips the live write gate
#   (mode.c set_mode_compression -> pm->set_compression) — confirmed 2026-07-02:
#   the validation SAVLIB landed LZO-compressed despite "enabled 0". Rather than
#   refuse DCE (host-visible; the IBM i expects an LTO-5 to compact), keep the
#   whole mode/log handshake intact and neuter ONLY the storage-side dispatch in
#   mhvtl_io.c writeBlock(): LZO/ZLIB cases -> writeBlock_nocomp(). Existing
#   compressed carts stay readable — decompression is chosen per-block from
#   on-media flags, not from the current mode setting.
IO="$SRC/usr/mhvtl_io.c"
[[ -f "$IO" ]] || { echo "Not found: $IO" >&2; exit 1; }
python3 - "$IO" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 9' in s:
    print("[=] Patch 9: write path already stores raw — nothing to do.")
    sys.exit(0)
old_lzo  = 'src_len = writeBlock_lzo(cmd, lbp_sz, FALSE, lbp_method);'
old_zlib = 'src_len = writeBlock_zlib(cmd, lbp_sz, FALSE, lbp_method);'
new_lzo  = ('src_len = writeBlock_nocomp(cmd, lbp_sz, FALSE, lbp_method); '
            '/* OpenVTL Patch 9: raw on media; the storage layer below (ZFS) dedups+compresses */')
new_zlib = ('src_len = writeBlock_nocomp(cmd, lbp_sz, FALSE, lbp_method); '
            '/* OpenVTL Patch 9 */')
if s.count(old_lzo) == 1 and s.count(old_zlib) == 1:
    s = s.replace(old_lzo, new_lzo, 1).replace(old_zlib, new_zlib, 1)
    open(p, 'w').write(s)
    print("[+] Patch 9: writeBlock() LZO/ZLIB dispatch now stores raw (nocomp).")
else:
    sys.stderr.write("[x] Patch 9: writeBlock() compression dispatch not found — inspect usr/mhvtl_io.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 10: init log_pg_list.description — vtltape exit-path SEGV ------------
#   alloc_log_page() (usr/mhvtl_log.c) mallocs a struct log_pg_list and fills
#   every field EXCEPT description. Nothing else ever sets it for log pages, so
#   it holds heap garbage for the daemon's whole life. The only reader is
#   dealloc_all_log_pages() — the cleanup_lu() exit path — which does
#   MHVTL_DBG(2, "Removing %s", lp->description): at verbose >= 2 printf walks a
#   garbage pointer and the daemon SEGVs INSIDE ITS SHUTDOWN. Cores captured
#   in the field (vtltape 1.8.0 @ 8e79aa8, -v3): printf <-
#   dealloc_all_log_pages(mhvtl_log.c:143) <- cleanup_lu(vtltape.c:2094) <- main.
#   This is the crash half of the v0.6 "pair-crash" — the exit itself is a stale
#   'vtlcmd N exit' message left in the persistent SysV queue (key 0x4d61726b)
#   by ExecStop + KillMode=none races; openvtld now flushes the queue during
#   library Apply (orchestrate/fc.go), so both halves are closed.
#   Fix: point description at the file's own log_page_desc[] table at alloc
#   time, and NULL-guard the dealloc DBG for any other construction path.
LOGC="$SRC/usr/mhvtl_log.c"
[[ -f "$LOGC" ]] || { echo "Not found: $LOGC" >&2; exit 1; }
python3 - "$LOGC" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 10' in s:
    print("[=] Patch 10: log page description already initialized — nothing to do.")
    sys.exit(0)
anchor = 'log_pg->log_subpage_num = subpage;'
init = (anchor +
        '\n\t\tlog_pg->description\t\t= (char *)(page < sizeof(log_page_desc) / sizeof(log_page_desc[0])'
        '\n\t\t\t\t\t\t\t\t\t\t? log_page_desc[page]'
        '\n\t\t\t\t\t\t\t\t\t\t: "Unknown Log page"); /* OpenVTL Patch 10 */')
old_dbg = 'MHVTL_DBG(2, "Removing %s", lp->description);'
new_dbg = ('MHVTL_DBG(2, "Removing %s", lp->description ? lp->description : "log page"); '
           '/* OpenVTL Patch 10 */')
if s.count(anchor) == 1 and s.count(old_dbg) == 1:
    s = s.replace(anchor, init, 1).replace(old_dbg, new_dbg, 1)
    open(p, 'w').write(s)
    print("[+] Patch 10: log_pg_list.description initialized; exit-path SEGV closed.")
else:
    sys.stderr.write("[x] Patch 10: alloc/dealloc anchors not found — inspect usr/mhvtl_log.c.\n")
    sys.exit(1)
PYEOF

# --- Patches 11-14: 3584 (TS3500) changer identity to GA32-0454-00 spec --------
#   Full spec diff and the post-patch wire capture live in the project's
#   3584 conformance record. All four patches are
#   SCOPED TO THE 3584 PERSONALITY FUNCTIONS in usr/pm/ibm_smc_pm.c — the
#   field-proven 3573-TL path stays byte-identical (operator requirement).
#   NB: after Patch 1 the 3573 function ALSO contains `start_map = 0x0300` and
#   the device-capabilities byte block is duplicated verbatim in both
#   personalities, so Patches 11 and 14 locate their function span first and
#   only rewrite inside it.

# --- Patch 11: init_ibm3584 I/E element start 0x0300 -> 0x0301 -----------------
#   A real 3584 assigns I/O slot 1 the import/export element address 769
#   (X'301'), not 768 (GA32-0454-00 p.100); drives 0x101 / storage 0x400 /
#   picker 0x1 already match the spec. Verified against a live capture
#   (mode page 0x1D bytes 10-11 = 03 00, RES type-3 first address 0x0300).
python3 - "$PM" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 11' in s:
    print("[=] Patch 11: 3584 I/E start already 0x0301 — nothing to do.")
    sys.exit(0)
i = s.find('void init_ibm3584(')
j = s.find('\n}', i)
if i < 0 or j < 0:
    sys.stderr.write("[x] Patch 11: init_ibm3584() not found — inspect usr/pm/ibm_smc_pm.c.\n")
    sys.exit(1)
body, n = re.subn(r'(start_map\s*=\s*)0x0300;',
                  r'\g<1>0x0301; /* OpenVTL Patch 11: I/O slot 1 = 769, GA32-0454-00 p.100 */',
                  s[i:j], count=1)
if n == 1:
    open(p, 'w').write(s[:i] + body + s[j:])
    print("[+] Patch 11: 3584 I/E element start -> 0x0301 (3573 untouched).")
else:
    sys.stderr.write("[x] Patch 11: start_map = 0x0300 not found inside init_ibm3584() — inspect usr/pm/ibm_smc_pm.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 12: update_ibm_3584_inquiry() to the spec'd FC identity -------------
#   Measured deviations (stock-mhVTL baseline capture): byte 4 = 0x35 (SPI length; FC is
#   0x33, bytes 56-57 not returned), byte 6 = 0x01 (NO BarC bit — a real 3584
#   reports BQue=1 + BarC=1 on FC), byte 7 = 0x20 (WBus16; FC is all-zero),
#   bytes 36-37 binary NULs (spec: ASCII plant-of-manufacture code), serial
#   bytes 38-49 left-justified space-padded (spec: right-justified, leading
#   zeroes). GA32-0454-00 p.37-38. VPD 0x83 copies its serial from
#   inquiry[38], so it inherits the fix.
python3 - "$PM" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 12' in s:
    print("[=] Patch 12: 3584 INQUIRY already spec-shaped — nothing to do.")
    sys.exit(0)
pat = re.compile(r'static void update_ibm_3584_inquiry\(struct lu_phy_attr \*lu\) \{.*?\n\}', re.S)
new = '''static void update_ibm_3584_inquiry(struct lu_phy_attr *lu) {
\tint n;

\tlu->inquiry[2] = 3; /* SNSI Approved Version */
\tlu->inquiry[3] = 2; /* Response data format */
\t/* OpenVTL Patch 12: GA32-0454-00 p.37-38, FC control path values:
\t * additional length 0x33 (SPI clocking bytes 56-57 not returned on FC),
\t * byte 6 BQue=1 + BarC=1, byte 7 all clear (WBus16/Sync/CmdQue), ASCII
\t * plant-of-manufacture code, serial right-justified w/ leading zeroes. */
\tlu->inquiry[4] = 0x33;  /* Additional length */
\tlu->inquiry[6] = 0xa0;  /* BQue=1, BarC=1 */
\tlu->inquiry[7] = 0x00;
\tlu->inquiry[36] = '0';  /* IBM Plant of Manufacture Code */
\tlu->inquiry[37] = '0';

\tn = (int)strnlen(lu->lu_serial_no, 12);
\twhile (n && lu->lu_serial_no[n - 1] == ' ')
\t\tn--;
\tmemset(&lu->inquiry[38], '0', 12);
\tmemcpy(&lu->inquiry[38 + 12 - n], lu->lu_serial_no, n);
\tlu->inquiry[50] = 0x30;
\tlu->inquiry[51] = 0x30;
}'''
s2, n = pat.subn(new, s, count=1)
if n == 1:
    open(p, 'w').write(s2)
    print("[+] Patch 12: 3584 INQUIRY -> FC identity (0x33/BarC/plant/right-just serial).")
else:
    sys.stderr.write("[x] Patch 12: update_ibm_3584_inquiry() not found — inspect usr/pm/ibm_smc_pm.c.\n")
    sys.exit(1)
PYEOF

# --- Patch 13: 3584 VPD 0x80 page shape + 0x83 stray-NUL fix -------------------
#   VPD 0x80 spec (GA32-0454-00 p.41): page length 0x10 — serial bytes 4-15
#   (12 ASCII, same right-justified zero-padded value as std INQUIRY 38-49) +
#   First Storage Element Address bytes 16-19. Stock emits length 0x16 with a
#   10-char LEFT-justified serial and an embedded NUL. Also: both vpd_80 and
#   vpd_83 write the address with snprintf, whose terminating NUL lands one
#   byte past the intended field (vpd_83: past the 0x2C data area) — memcpy a
#   4-char scratch instead. init_ibm3584 populates the std INQUIRY before the
#   VPD pages, so copying inquiry[38] picks up the Patch 12 serial.
python3 - "$PM" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 13' in s:
    print("[=] Patch 13: 3584 VPD 0x80/0x83 already spec-shaped — nothing to do.")
    sys.exit(0)
old_alloc = 'lu_vpd[pg] = alloc_vpd(0x16);'
new_alloc = 'lu_vpd[pg] = alloc_vpd(0x10); /* OpenVTL Patch 13: page length 0x10, GA32-0454-00 p.41 */'
old_err   = 'MHVTL_ERR("Could not malloc(0x16) bytes, line %d", __LINE__);'
new_err   = 'MHVTL_ERR("Could not malloc(0x10) bytes, line %d", __LINE__);'
old_body = '''\t/* d[4 - 15] Serial number of device */
\tsnprintf((char *)&d[0], 11, "%-10.10s", lu->lu_serial_no);
\t/* First Storage Element Address */
\tsnprintf((char *)&d[12], 5, "%04x", smc_p->pm->start_storage);'''
new_body = '''\t/* OpenVTL Patch 13: serial bytes 4-15 = std INQUIRY bytes 38-49
\t * (12 ASCII, right-justified, leading zeroes; populated first in
\t * init_ibm3584); First Storage Element Address bytes 16-19 as four
\t * ASCII hex digits, memcpy'd so no stray NUL. GA32-0454-00 p.41. */
\tmemcpy(&d[0], &lu->inquiry[38], 12);
\t{
\t\tchar a4[5];

\t\tsnprintf(a4, sizeof(a4), "%04x", smc_p->pm->start_storage);
\t\tmemcpy(&d[12], a4, 4);
\t}'''
old_83 = '\tsnprintf((char *)&d[40], 5, "%04x", smc_p->pm->start_storage);'
new_83 = '''\t{
\t\tchar a4[5]; /* OpenVTL Patch 13: no snprintf NUL past the 0x2C data area */

\t\tsnprintf(a4, sizeof(a4), "%04x", smc_p->pm->start_storage);
\t\tmemcpy(&d[40], a4, 4);
\t}'''
# Normalize: the source indents with tabs; match against the file as-is but
# tolerate the exact block only. All three anchors must be unique.
ok = s.count(old_alloc) == 1 and s.count(old_body) == 1 and s.count(old_83) == 1
if not ok:
    sys.stderr.write("[x] Patch 13: vpd_80/vpd_83 anchors not found or ambiguous — inspect usr/pm/ibm_smc_pm.c.\n")
    sys.exit(1)
s = s.replace(old_alloc, new_alloc, 1).replace(old_body, new_body, 1).replace(old_83, new_83, 1)
if s.count(old_err) == 1:
    s = s.replace(old_err, new_err, 1)
open(p, 'w').write(s)
print("[+] Patch 13: 3584 VPD 0x80 -> length 0x10 + INQUIRY-serial; 0x80/0x83 address NULs gone.")
PYEOF

# --- Patch 14: update_3584_device_capabilities() -> spec page shape ------------
#   GA32-0454-00 p.57: mode page 0x1F parameter length is 0x0E (16 bytes
#   total) and byte 12 (Medium Transport Element EXCHANGE capabilities) is
#   0x00 — the 3584 robot does not exchange. Stock mhVTL serves a 20-byte
#   page (length 0x12) with byte 12 = 0x0E. The byte block is duplicated
#   verbatim in the 3573 function, so rewrite only inside the 3584 span.
python3 - "$PM" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 14' in s:
    print("[=] Patch 14: 3584 device capabilities already spec-shaped — nothing to do.")
    sys.exit(0)
i = s.find('static void update_3584_device_capabilities(')
j = s.find('\n}', i)
if i < 0 or j < 0:
    sys.stderr.write("[x] Patch 14: update_3584_device_capabilities() not found — inspect usr/pm/ibm_smc_pm.c.\n")
    sys.exit(1)
body = s[i:j]
old_12 = '\tmp->pcodePointer[12] = 0x0e; /* Medium Transport Capabilities */'
new_12 = '\tmp->pcodePointer[12] = 0x00; /* OpenVTL Patch 14: MT exchange = none, GA32-0454-00 p.57 */'
if body.count(old_12) != 1:
    sys.stderr.write("[x] Patch 14: byte-12 anchor not found inside update_3584_device_capabilities() — inspect usr/pm/ibm_smc_pm.c.\n")
    sys.exit(1)
body = body.replace(old_12, new_12, 1)
body += '''
\t/* OpenVTL Patch 14: the real 3584 serves a 14-byte-parameter page
\t * (length 0x0E, 16 bytes total); trim the generic 20-byte alloc so
\t * an all-pages MODE SENSE carries no phantom trailing bytes. */
\tmp->pcodeSize = 16;
\tmp->pcodePointer[1] = 0x0e;
\tmp->pcodePointerBitMap[1] = 0x0e;'''
open(p, 'w').write(s[:i] + body + s[j:])
print("[+] Patch 14: 3584 mode page 0x1F -> length 0x0E, MT exchange 0x00 (3573 untouched).")
PYEOF

# --- Patch 15: 3584 VPD page 0xD0 — IBM i changer type classification ----------
#   Without a 0xD0 page the IBM i classifies the 03584L32 changer as a
#   GENERIC library (WRKHDWRSC *STG shows 9429-310) even though vary on +
#   SAVLIB work (field result). Precedent: the 3573 only
#   classifies as 3573-020 because of ITS 0xD0 page (Patch 2 history), and a
#   real 3584 advertises 0xD0 too (GA32-0454-00 p.40; contents "not
#   specified"). The 3573's page (raw dump from real hardware, in
#   ibm_smc_pm.c) decodes as a 5-byte "SCDD" signature + TLV records; record
#   0x000B carries the machine type as BCD: 80 00 00 00 [35 73] 00 40.
#   This patch clones that page for the 3584 personality with the type bytes
#   set to 35 84 and everything else identical.
#   ✅ CONFIRMED on a live IBM i: WRKHDWRSC now shows the MLB as 3584-040
#   and it varies on — TLV 0x000B is the type+model, both BCD: bytes 4-5 =
#   machine type (35 84), bytes 6-7 = model (00 40 -> "040", inherited from
#   the TS3100 dump). A 3584-403 identity is therefore bytes 04 03 in the
#   model field — built as Patch 16, then retired 2026-08-24 when OpenVTL
#   went FC-only (the 403 is IBM i's iSCSI VTL model).
python3 - "$PM" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 15' in s:
    print("[=] Patch 15: 3584 VPD 0xD0 already present — nothing to do.")
    sys.exit(0)

# The TS3100 0xD0 payload (192 bytes, header stripped), byte-identical to
# the pg_d0[] dump in this same file, with TLV 0x000B's BCD machine type
# changed 35 73 -> 35 84. Regenerated from the canonical hex so the bytes
# cannot drift from what is documented.
hex3573 = (
    "04 53 43 44 44 00 04 04 c0 00 00 00 00 06 04 22"
    "80 00 00 00 08 07 80 00 00 05 00 00 0a 00 0b 08"
    "80 00 00 00 35 73 00 40 00 0e 02 00 ff 00 0f 01"
    "ff 00 10 02 00 ff 00 11 10 04 8e 82 72 04 83 82"
    "75 3b 12 82 75 04 12 82 76 00 14 3a 00 01 03 01"
    "07 0b 12 01 15 01 16 01 17 01 1a 01 1b 0a 1d 01"
    "1e 01 2b 0a 37 0b 3b 03 3c 01 4c 01 4d 01 55 01"
    "56 01 57 01 5a 01 5e 01 5f 01 a3 01 a4 01 a5 19"
    "b5 01 b6 01 b8 01 00 16 03 80 24 02 00 17 11 00"
    "00 10 20 06 01 00 10 20 06 01 01 00 00 00 00 00"
    "00 1b 01 60 00 1c 05 00 00 06 06 02 00 24 0c 83"
    "00 83 03 83 30 83 11 3b 12 83 02 00 26 02 00 05")
pg = bytearray(bytes.fromhex(hex3573.replace(" ", "")))
assert len(pg) == 0xc0, len(pg)
assert pg[36] == 0x35 and pg[37] == 0x73, "TLV 0x000B type bytes not where expected"
pg[37] = 0x84  # BCD 3573 -> 3584

lines = []
for i in range(0, len(pg), 8):
    chunk = ", ".join("0x%02x" % b for b in pg[i:i+8])
    lines.append("\t\t" + chunk + ",")
arr = "\n".join(lines)
arr = arr.rstrip(",")

func = '''
/*
 * OpenVTL Patch 15: VPD page 0xD0 for the 3584 personality. A real 3584
 * advertises 0xD0 (GA32-0454-00 p.40, contents unspecified); without it
 * the IBM i classifies the changer as a generic 9429-310 (field result
 * 2026-07-18) instead of a 3584. Page cloned from the TS3100 raw dump
 * above with TLV record 0x000B's BCD machine type set to 3584.
 */
static void update_ibm_3584_vpd_d0(struct lu_phy_attr *lu) {
\tstruct vpd **lu_vpd = lu->lu_vpd;
\tuint8_t\t\t*d;
\tint\t\t\t pg;
\tuint8_t\t\t pg_d0[] = {
%s
\t};

\tpg = PCODE_OFFSET(0xd0);
\tif (lu_vpd[pg]) /* Free any earlier allocation */
\t\tdealloc_vpd(lu_vpd[pg]);
\tlu_vpd[pg] = alloc_vpd(sizeof(pg_d0));
\tif (!lu_vpd[pg]) {
\t\tMHVTL_ERR("Could not malloc(0xc0) bytes, line %%d", __LINE__);
\t\treturn;
\t}

\td = lu_vpd[pg]->data;
\tmemcpy(d, pg_d0, sizeof(pg_d0)); /* form differs from the 3100 fn so Patch 2 can never match it */
}

''' % arr

anchor_fn = "/*\n * This should be fun to keep in sync..\n"
anchor_call = "\tupdate_ibm_3584_vpd_83(lu);\n\t/* IBM Doco hints at VPD page 0xd0 - but does not document it */"
new_call = ("\tupdate_ibm_3584_vpd_83(lu);\n"
            "\tupdate_ibm_3584_vpd_d0(lu); /* OpenVTL Patch 15: IBM i type classification */")
if s.count(anchor_fn) != 1 or s.count(anchor_call) != 1:
    sys.stderr.write("[x] Patch 15: anchors not found in usr/pm/ibm_smc_pm.c — inspect manually.\n")
    sys.exit(1)
s = s.replace(anchor_fn, func + anchor_fn, 1).replace(anchor_call, new_call, 1)
open(p, 'w').write(s)
print("[+] Patch 15: 3584 personality now serves VPD 0xD0 (type BCD 3584).")
PYEOF

# --- Patch 16: 3584-403 model variant (product id 03584403) — RETIRED, dormant -
#   Retired 2026-08-24: the 3584-403 is IBM i's certified iSCSI VTL identity,
#   and OpenVTL ships FC-only. The patch is kept (dormant) so already-patched
#   trees stay stable and idempotent re-runs stay green; nothing creates a
#   03584403 library any more. Rationale, self-contained: VPD D0 TLV record
#   0x000B carries the type-model in BCD (Patch 15), so the 403 is model bytes
#   04 03, keyed on the OpenVTL-defined product id 03584403 (vtllibrary routes
#   any 03584* to this personality). Every other 3584 keeps model 040.
python3 - "$PM" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
if 'OpenVTL Patch 16' in s:
    print("[=] Patch 16: 3584-403 model override already present — nothing to do.")
    sys.exit(0)
# (a) D0 model override after the page copy (Patch 15's function; its
# memcpy form is deliberately distinct from anything Patch 2 matches).
old_copy = ('\td = lu_vpd[pg]->data;\n'
            '\tmemcpy(d, pg_d0, sizeof(pg_d0)); /* form differs from the 3100 fn so Patch 2 can never match it */\n}')
new_copy = ('\td = lu_vpd[pg]->data;\n'
            '\tmemcpy(d, pg_d0, sizeof(pg_d0)); /* form differs from the 3100 fn so Patch 2 can never match it */\n'
            '\t/* OpenVTL Patch 16: product id 03584403 = the iSCSI VTL variant;\n'
            '\t * TLV 0x000B model bytes -> BCD 403 (IBM i reports 3584-403). */\n'
            '\tif (!strncasecmp(lu->product_id, "03584403", 8)) {\n'
            '\t\td[38] = 0x04;\n'
            '\t\td[39] = 0x03;\n'
            '\t}\n'
            '}')
# (b) teach init_ibm3584's model-sanity logging about the new id so a
# 403 library doesn't log "library model not known" at every start.
old_log = '} else if (!strncasecmp(lu->product_id, "03584L32", 8)'
new_log = ('} else if (!strncasecmp(lu->product_id, "03584403", 8) /* OpenVTL Patch 16 */ '
           '|| !strncasecmp(lu->product_id, "03584L32", 8)')
if s.count(old_copy) != 1 or s.count(old_log) != 1:
    sys.stderr.write("[x] Patch 16: anchors not found in usr/pm/ibm_smc_pm.c — inspect manually.\n")
    sys.exit(1)
s = s.replace(old_copy, new_copy, 1).replace(old_log, new_log, 1)
open(p, 'w').write(s)
print("[+] Patch 16: product 03584403 -> D0 model BCD 403 (3584-403 iSCSI VTL).")
PYEOF
