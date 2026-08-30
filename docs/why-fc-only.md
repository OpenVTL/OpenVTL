# Why OpenVTL is Fibre Channel only

OpenVTL attaches to IBM i over Fibre Channel, and only over Fibre Channel.
The product presents its two library models — TS3100/TS3200 (3573) and
TS3500 (3584) — as FC targets behind a QLogic HBA. There is no iSCSI attach path.
This page records why: the answer is not "it was never built".

## iSCSI was built, investigated, and removed

An iSCSI attach of the virtual 3584 to IBM i was fully implemented — target
plumbing, per-host ACLs, a dedicated library variant with the correct wire
identity — and put through a complete investigation in August 2026. It was
then removed from the product. What follows is a summary of what that
investigation found.

## What the investigation found

Across two IBM i releases (7.4, and 7.5 at current PTF levels) on two
different Power platforms, every **successful** iSCSI login negotiation from
the host's 298A-001 iSCSI virtual IOP was followed by a reset of that IOP,
with matching SRCs each time. Fourteen captured login exchanges show a
consistent picture:

- The reset follows the login response and happens **before any SCSI command
  flows** — no INQUIRY, no REPORT LUNS, no I/O is ever issued.
- The failure is **content-independent**: login responses with different
  response bytes end identically. Login *rejections* never trigger it — only
  successful logins do.
- It reproduces against a **completely stock Linux LIO target**, with no
  OpenVTL software in the path, and against every target-side accommodation
  the investigation built and tested.
- The login exchanges were independently re-checked against RFC 3720 and are
  conformant.
- A **PASE-initiated control-path login** from the same host to the identical
  target, ACL, and portal succeeds cleanly.

## The conclusion

Nothing on the target side prevents or fixes the failure. Since no change
OpenVTL can ship alters the outcome, shipping an iSCSI attach for IBM i would
mean shipping a broken attach path and letting customers find that out in
production. So the product does not ship one.

The Fibre Channel path, by contrast, is field-validated end to end: the
TS3500 (3584) classifies as a 3584 in WRKHDWRSC, varies on, and commits
saves over both direct and fabric-switch attach, held to the IBM 3584
SCSI reference at the wire level — and the TS3100/TS3200 (3573) path is
proven the same way.

## One fabric

iSCSI attach for other hosts (Linux and Windows initiators) worked, and was
removed anyway. With IBM i — the platform OpenVTL is built for — settled on
FC, keeping a second transport for secondary hosts would have doubled the
attach surface, the test matrix, and the security review for a path the
primary workload cannot use. OpenVTL ships single-fabric: one transport,
fully validated.
