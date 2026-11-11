# Deploying mapthing: Docker Compose behind a Cloudflare Tunnel

This is the reference setup: one mapthing container and a `cloudflared` sidecar on any Docker
host. The tunnel publishes the server at your own hostname over HTTPS; the host needs no open
ports, no public IP and no certificate. The files are in [`samples/`](../samples/).

Any reverse proxy works the same way (Caddy, nginx, Traefik): terminate TLS, forward to
`http://mapthing:8080`, pass `X-Forwarded-For` and `X-Forwarded-Proto`, and do not buffer
responses (the admin UI uses server-sent events). The settings below apply unchanged.

## What you need

- A Docker host with Compose v2. For serving, 2 vCPU and 4 GB are comfortable. **Builds run in
  the same container and need more**: memory, and free disk of about six times the OpenStreetMap
  extracts you pick, plus the elevation data. [sizing.md](sizing.md) has the figures by country;
  the admin UI shows an estimate before you add a region and refuses a build that will not fit.
- A domain on Cloudflare (the free plan is enough) and access to its Zero Trust dashboard.

## 1. Create the tunnel

1. Cloudflare dashboard → **Zero Trust** → **Networks** → **Tunnels** → **Create a tunnel**.
2. Choose **Cloudflared**, name it (for example `maps`), and save.
3. On the "Install connector" page, copy the **token**: the long string after `--token` in the
   command it shows. You do not run that command; the compose file runs the connector.
4. On the **Public Hostname** tab, add a hostname:
   - **Subdomain / Domain**: for example `maps` . `example.com`.
   - **Service**: type `HTTP`, URL `mapthing:8080`.

   Cloudflare creates the DNS record. `mapthing` is the container's name on the compose
   network, which is how the connector reaches it.

## 2. Put the files on the host

```sh
sudo mkdir -p /docker/maps && cd /docker/maps
curl -fsSLO https://raw.githubusercontent.com/jclement/mapthing/main/samples/docker-compose.yaml
curl -fsSL  https://raw.githubusercontent.com/jclement/mapthing/main/samples/.env.example -o .env
chmod 600 .env
```

## 3. Fill in `.env`

| Setting | Value |
|---|---|
| `ADMIN_USER`, `ADMIN_PASSWORD` | The login for the admin UI. Use a long random password: this page is on the internet. |
| `MAPTHING_PUBLIC_URL` | `https://` plus the hostname from step 1, no trailing slash. It is written into every style and TileJSON the server hands out, and into the snippets shown when you create a key. |
| `MAPTHING_TRUSTED_PROXY` | `on` (the sample's value). Every request arrives through the tunnel, so the client's address is taken from the tunnel's headers. Login throttling, rate limits and logs then see real clients. Set it to `off` if you ever publish port 8080 to clients directly. |
| `TUNNEL_TOKEN` | The token from step 1. |
| `MAPTHING_TAG` | Optional. Leave unset to follow `latest`; set it to a `sha-…` tag to stay on one version (see [Updating](#updating)). |

Every other setting has a default that suits this setup. The admin **Settings** page lists them
all with their current values; the table in the [README](../README.md#running) covers the ones
people change.

## 4. Create the data folder

Everything mapthing keeps is in `./data`: downloads, built generations, the database (keys,
schedule, usage), caches and logs. The container runs as user 10001, which must own it:

```sh
mkdir -p data && sudo chown 10001:10001 data
```

Put it on the disk with the room: it is where builds happen.

## 5. Start it

```sh
docker compose up -d
docker compose ps          # mapthing: healthy after about 30 s; mapthing-tunnel: running
docker compose logs -f     # everything the server, its processes and builds print
```

Open `https://<your hostname>/admin` and sign in. On the host itself the server also answers at
`http://127.0.0.1:8080/admin` (the compose file publishes the port on the loopback address
only), which is the way in if the tunnel is down.

## 6. Build your first map

The **Overview** page says what to do next at each step:

1. **Regions** → **Add a region**: pick a country, state or province from the catalog, or draw
   a box. The dialog shows the download size and the disk a build needs.
2. **Builds** → **Build now**. The first build downloads the OpenStreetMap extract and the
   elevation data, then makes the vector tiles, the routing graph and the terrain. A US state
   takes from minutes to a few hours; progress and the log are on the page.
3. **API keys** → **Create key**. The key is shown once, with ready-made URLs for MapLibre or
   Mapbox GL, raster tiles and the Google-compatible endpoints.

After that the **Schedule** page keeps the data fresh: weekly by default, and each region can
have its own setting (daily, weekly, monthly, a cron expression, or never), a build window such
as nights only, and "only when the source has something new".

## Checking it works

```sh
curl -fsS https://maps.example.com/healthz          # {"ok":true,"version":"…"}
curl -fsS https://maps.example.com/readyz           # 200 once a map is live and routing is up
curl -fsS -H "Authorization: Bearer $KEY" https://maps.example.com/tiles/basemap.json
```

In the admin UI, **Overview** shows every process, traffic and latency by kind, and disk against
what the next build needs. **Logs** has everything `docker compose logs` has, searchable.

## Updating

```sh
cd /docker/maps
docker compose pull && docker compose up -d
```

That replaces the container with the newest image; `./data` is untouched and the server comes
back serving the same generation within about 30 seconds (routing a little later, once its
graph is loaded). Requests during those seconds fail, so do it at a quiet time.

**A build that is running when the container is replaced is lost.** Nothing is damaged: the
live generation keeps serving after the restart, the downloads are kept, and the half-written
generation is removed by the next build. The build shows as failed ("interrupted by server
restart") in Builds, and is not restarted by itself: start it again, or wait for the next
scheduled one. Check **Overview** → Builds → "Now" before updating; it says whether one is
running.

**Staying on one version.** `latest` moves with every change on `main`. To choose when you
update, set `MAPTHING_TAG` in `.env` to a published `sha-<commit>` tag
([the image's tags](https://github.com/jclement/mapthing/pkgs/container/mapthing)) and change it
when you want to move.

## Rolling back

There are two different things to roll back.

- **The software.** Set `MAPTHING_TAG` in `.env` to the tag you ran before and
  `docker compose up -d`. The version you are running is at the top of **Overview** and in
  **Settings** → Software. The data folder works with older and newer versions alike: a newer
  version only ever adds database tables and columns, which an older one ignores.
- **The map data.** A bad build never goes live: every build is checked (tiles readable, a
  route and an elevation per region) before it replaces the live generation. If a build that
  passed still turns out wrong, **Generations** → the previous one → **Roll back to this**.
  The switch has no downtime. The previous generation is kept until the next successful build.

## Backups

Stop-free and small: copy `data/mapthing.db` (API key hashes, regions, schedule, usage,
history). `sqlite3 data/mapthing.db ".backup backup.db"` gives a consistent copy while the
server runs. Everything else in `data/` can be rebuilt from the regions list by building again.
Key secrets are not recoverable from a backup or anywhere else: only their hashes are stored.

## When something is wrong

| Symptom | Look at |
|---|---|
| The hostname gives a Cloudflare error (502, 1033) | `docker compose ps` and `docker compose logs cloudflared`. The token is wrong, or the public hostname's service is not `http://mapthing:8080`. |
| Signing in answers "cross-origin request refused" | The proxy rewrites the `Host` header. Pass the public host through, or set `MAPTHING_PUBLIC_URL` to the address in the browser. |
| "too many failed logins" for everyone at once | `MAPTHING_TRUSTED_PROXY` is off, so every visitor shares the tunnel's address. |
| The map is blank in a client app | The style's tile URLs: they are built from `MAPTHING_PUBLIC_URL`. Check the key's allowed origins too. |
| A build fails at once | Its log (Builds → the failed row → "See what went wrong in the log"). The usual cause is disk: Overview → Resources shows what the next build needs. |
| A build was killed part-way | The machine ran out of memory (`dmesg` shows the kernel's kill). Give the container more memory or build smaller regions; see [sizing.md](sizing.md). |
| `permission denied` on `/data` at start | `sudo chown 10001:10001 data`. |
| Settings shows a variable under "Not recognised" | Its name is misspelt in `.env`; the real setting is still at its default. |
