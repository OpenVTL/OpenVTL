# Security policy

## Reporting a vulnerability

**Do not open a public issue for a suspected vulnerability.**

Email **security@openvtl.com** with:

- the OpenVTL version and how the appliance is deployed (FC topology,
  network exposure of the UI),
- a description and, if possible, reproduction steps or a proof of
  concept,
- your assessment of impact.

You will receive an acknowledgement within **3 business days**. We ask
for a coordinated disclosure window of **90 days** from acknowledgement
(shorter by agreement once a fix ships; longer only if we ask and you
agree). We will credit reporters in the release notes unless you prefer
otherwise.

Fixes ship as signed release bundles through the normal update channel
(`openvtld update` / Settings → Updates). There is no bug bounty at this
time.

## Scope

In scope: the `openvtld` daemon (API, auth, updater, S3 handling), the
embedded web UI, the installer and packaging, and the release
signing/verification chain.

**Deployment assumptions.** The management UI and API are designed to
run on an isolated management network and are not hardened for exposure
to untrusted networks or the internet. Network-level access control, and
any authentication hardening beyond the built-in session auth, are the
operator's responsibility — reports that assume direct internet exposure
of `:8443`/`:8080` will be assessed against this deployment model.

**Not security reports — different route:**

- **Emulation behavior.** The data path (mhVTL patches, LIO layout, SCSI
  identity) is deliberately frozen; deviations visible to the host
  (BRMS, WRKMLBSTS, sense data) are *conformance* issues. Report them as
  ordinary public issues with wire-level evidence (the command, the
  INQUIRY/VPD or element-status bytes observed, and what the IBM
  reference expects) — unless the deviation itself has a security
  consequence (e.g. data written to the wrong cartridge), in which case
  use security@openvtl.com.
- Operational problems and questions: public issues per
  [CONTRIBUTING.md](CONTRIBUTING.md) (no support bundles publicly), or
  the support portal for support customers.

## Hardening notes for reporters

The UI listens on `:8443` (TLS, authenticated); `:8080` is
health/metrics only and can be disabled (`-listen ""`). `/metrics` is
deliberately unauthenticated — reports that it is reachable on a
management network are expected behavior, not findings.
