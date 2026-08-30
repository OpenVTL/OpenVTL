# Contributing to OpenVTL

## Contributor License Agreement — required

Outside contributions can only be accepted from contributors who have signed
the [OpenVTL CLA](CLA.md). To sign, add a row with your name, GitHub handle,
and the date to [`cla-signatures.md`](cla-signatures.md) — either as the
first commit of your first pull request, or as a separate PR. The commit
must come from your own account; that commit in the repository history is
the durable record of your agreement:

> I have read the OpenVTL CLA (CLA.md) and I agree to its terms for my present
> and future contributions.

PRs from contributors without a row in `cla-signatures.md` will be held
until one is on file.

The CLA is **individual-only**. If you would be contributing on behalf
of an employer or other entity, contact legal@openvtl.com before
submitting.

## Licensing of contributions

- Daemon (`openvtld/`), web UI (`openvtld/web/`), packaging and scripts:
  **AGPL-3.0** (see [LICENSE](LICENSE)).
- Changes to the mhVTL patch set (`packaging/mhvtl/`): **GPL-2.0**, since they
  are derivative works of mhVTL (see
  [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)).

## Development

- The daemon is Go (`openvtld/`); build with
  `cd openvtld && go build ./cmd/openvtld`, test with `go test ./...`.
- The web UI is React + Vite (`openvtld/web/`); see its README for the dev
  loop. `npm run build` output is embedded into the daemon binary.
- The data path (mhVTL patches, LIO layout, SCSI identity) is **frozen** —
  BRMS-visible behavior must not change. Emulation changes need
  wire-level conformance evidence against the IBM SCSI references
  (INQUIRY/VPD pages, element status, sense data, and behavior on a real
  IBM i attach) before they can be considered.
- Release bundles are cut with `scripts/make-release.sh` and are
  Ed25519-signed; unsigned bundles are refused by the installer and updater.

## Reporting issues

**Do not attach support bundles to public issues.** They are redacted of
credentials, but they still identify an installation: support key, system
UUID, hostname, device serials, S3 endpoint and bucket names, and client
addresses. In a public issue, include instead:

- the OpenVTL version (`openvtld -version` or Settings → System),
- library model(s) and drive type, and a one-line topology sketch
  (e.g. "TS3100/TS3200 (3573), 2× LTO-5, single FC path"),
- what you did, what you expected, what happened,
- the relevant `journalctl -u openvtld` excerpt **after removing
  hostnames, WWPNs, serials, and addresses**.

If a full support bundle is needed to triage, we will ask for it through
the private support channel (see [SECURITY.md](SECURITY.md) for security
reports; support customers use their support portal) — never post one
publicly.

Suspected **security vulnerabilities** must not be filed as public issues
at all — see [SECURITY.md](SECURITY.md).
