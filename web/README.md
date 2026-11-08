# mapthing admin UI

React + Vite + TypeScript + Tailwind v4 + MapLibre GL JS (terra-draw for drawing boxes).
Built into `web/dist` and embedded in the Go binary; served at `/admin/`.

```sh
npm ci
npm run dev:mock   # no backend needed: in-memory mock server (sign in with admin / admin
                   # when you open /admin/?login; otherwise you start signed in)
npm run dev        # proxies /admin/api, /api, /styles, /tiles … to MAPTHING_URL (default :8080)
npm test           # vitest (jsdom): login, key secret shown once, add-region flow, cron preview
npm run lint       # tsc --noEmit + biome
npm run fmt        # biome --write
npm run build      # → dist/
npm run screens    # Playwright screenshots of every page against a running dev:mock (port 5181)
```

Mock-mode URL switches: `?empty` (first run, nothing installed), `?login` (start signed out).

- `src/api.ts`: typed client for the admin API and the keyed endpoints (shapes documented in
  `docs/architecture.md`, "Admin UI and admin API shapes").
- `src/mocks/`: the mock backend (fetch + EventSource shims, fixtures, a simulated build). It is
  only imported when `VITE_MOCK` is set (mock and test modes), so it is not in the production bundle.
- `src/pages/`: one file per page. `src/components/`: UI kit, charts, map wrapper, style picker.
