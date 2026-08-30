# openvtld web UI

The management UI embedded in the openvtld daemon — React 19 + Vite + Tailwind CSS 4.

## Development

```sh
npm ci
VTL_API=https://<appliance>:8443 npm run dev
```

Vite proxies `/api` (and `/metrics`) to the appliance named by `VTL_API`
(default `https://127.0.0.1:8443`), so the dev UI runs against live data.

## Build

`npm run build` writes `dist/`, which is embedded into the openvtld binary
at release cut (copied to `openvtld/internal/api/dist`).
