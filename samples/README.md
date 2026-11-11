# Sample deployment: one container behind a Cloudflare Tunnel

Reference files for running mapthing on any Docker host and publishing it through a Cloudflare
Tunnel, so the host needs no open ports or public IP.

| File | |
|---|---|
| `docker-compose.yaml` | mapthing (`ghcr.io/jclement/mapthing`) and a `cloudflared` sidecar |
| `.env.example` | admin login, public URL, tunnel token; copy to `.env` |
| `auto-update/` | opt-in: a script and a systemd timer that update the container nightly, never during a build |

The full guide, with updating, rolling back, backups and troubleshooting, is
[docs/deploy.md](../docs/deploy.md). In short:

## Steps

1. **Create the tunnel.** Cloudflare dashboard: Zero Trust > Networks > Tunnels > Create a tunnel >
   Cloudflared. Add a public hostname (for example `maps.example.com`) with service
   `http://mapthing:8080`. Cloudflare creates the DNS record. Copy the tunnel token.
2. **On the host:**
   ```sh
   mkdir -p /docker/maps && cd /docker/maps
   curl -fsSLO https://raw.githubusercontent.com/jclement/mapthing/main/samples/docker-compose.yaml
   curl -fsSL  https://raw.githubusercontent.com/jclement/mapthing/main/samples/.env.example -o .env
   chmod 600 .env          # set ADMIN_PASSWORD, TUNNEL_TOKEN and MAPTHING_PUBLIC_URL
   mkdir -p data && sudo chown 10001:10001 data
   docker compose up -d
   ```
3. Open `https://<your hostname>/admin`, add regions and build. On the host itself the server is
   also at `http://127.0.0.1:8080`.

**Update:** `docker compose pull && docker compose up -d`. Overview says when a newer image
exists. A build that is running is lost (the live map is not affected): check Overview first.
To pin a release, set `MAPTHING_TAG` (for example `sha-242aefa`) in `.env`. To have it done
for you, see `auto-update/` and "Updating by itself" in [docs/deploy.md](../docs/deploy.md).

**Sizing:** serving is small (2 vCPU and 4 GB is comfortable). Building runs in the same container
and needs more: see [docs/sizing.md](../docs/sizing.md).
