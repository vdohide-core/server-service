# server-service

Embed player + content delivery server for vdohide-core.  
Serves the video embed page (`/embed/{slug}`), VAST ad tags, HLS streams, images, sprites, and thumbnails from storage nodes.  
Also runs background schedulers including the **Hetzner Auto-Scaler** for dynamic download worker management.

---

## Features

### Player
- Embed player page (`/embed/{slug}`) with domain-aware configuration
- VAST 3.0 ad tag endpoint (`/vast.xml`)
- Space-based access control — domain scoped to a `spaceId`
- Plan-aware ad resolution: `free` plan → global ads, `paid` plan → domain ads
- Maintenance mode via `player_maintenance` setting
- Processing status page: queue / processing (with %) / error states

### Content Delivery
- HLS master playlist (`/{slug}/playlist.m3u8`)
- HLS segment playlist (`/{mediaSlug}/video.m3u8`) with CDN domain rewriting
- Image proxy with on-the-fly resize (`?w=400&h=300&fit=cover&q=80`)
- Sprite sheet + VTT proxy (`/{slug}/sprite/sprite.vtt`, `/{slug}/sprite/{n}.jpg`)
- Thumbnail poster proxy (`/thumb/{slug}/{n}.jpg`)
- Image not-found PNG placeholder

### Infrastructure
- Settings sync every 1 minute from MongoDB → `conf/setting.json`
- Domain cache sync → `conf/domains.json`
- Space cache sync → `conf/spaces.json`
- Rotating log file (25 MB per file, startup rotation)
- Log reader API (`GET /logs`, `GET /logs/{filename}?tail=200`)
- **Web UI** (`/ui`) — real-time log viewer + Hetzner server dashboard

### Hetzner Auto-Scaler
- Runs every 1 minute, config loaded from MongoDB `hetzner_auto_scale` setting
- Counts pending download jobs from `files` collection:
  - `status: "waiting"` + `clonedFrom` not set + not trashed/deleted
  - No existing `medias` or `video_process` records
  - Has `ingest` record **or** `metadata.source` set
- Scales UP: `ceil(pending / downloadsPerServer)` servers, max `maxServers`
  - Skips creation if any server is still `initializing`
  - Server name: `vdohide-dl-{timestamp}-{i}` (unique per batch)
  - SSH keys injected at creation for remote access
- Scales DOWN: deletes servers at billing hour boundary (minute 55–59) when idle
  - Each server checked individually — busy servers are skipped
  - Waits for next billing hour if still processing
  - Checks `video_process.workerId` matching `{serverName}@N` before deletion
- Cloud-Init: runs install script via `user_data` on boot

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8083` | HTTP listen port |
| `MONGODB_URI` | _(required)_ | MongoDB connection string |
| `LOG_PATH` | `logs/server-content.log` | Log file path |

---

## Settings (MongoDB `settings` collection)

| Name | Type | Description |
|---|---|---|
| `player_maintenance` | `boolean` | Enable maintenance mode (`false` = normal) |
| `advert_vdo` | `array` | Global video ad list (free plan VAST) |
| `advert_image` | `object` | Global image overlay ad (free plan) |
| `advert_javascript` | `string` | Global JS ad code (free plan) |
| `hetzner_auto_scale` | `object` | Hetzner Auto-Scaler configuration (see below) |
| `hetzner_ssh_private_key` | `string` | PEM private key for SSH log viewer |

### `hetzner_auto_scale` shape
```json
{
  "enabled": true,
  "apiToken": "...",
  "serverType": "cx22",
  "location": "fsn1",
  "maxServers": 10,
  "downloadsPerServer": 2,
  "idleMinutes": 2,
  "deletionWindowMinutes": 5,
  "mongoUri": "mongodb+srv://...",
  "storagePath": "/home/files",
  "installUrl": "https://raw.githubusercontent.com/vdohide-core/server-download/main/install.sh",
  "sshKeys": ["rsa-key-20230228"]
}
```

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable/disable the scaler |
| `apiToken` | _(required)_ | Hetzner Cloud API token |
| `serverType` | `cx22` | Hetzner server type |
| `location` | `nbg1` | Hetzner datacenter location |
| `maxServers` | `10` | Maximum concurrent servers |
| `downloadsPerServer` | `2` | Concurrent downloads per server |
| `idleMinutes` | `10` | Minimum idle time before checking billing boundary |
| `deletionWindowMinutes` | `5` | Minutes before billing hour end to delete (e.g. 5 = delete at minute 55–59) |
| `mongoUri` | _(required)_ | MongoDB URI passed to install script |
| `storagePath` | `/home/files` | Storage path passed to install script |
| `installUrl` | _(GitHub raw URL)_ | Install script URL |
| `sshKeys` | `[]` | Hetzner SSH key names/IDs to add to new servers |

### `hetzner_ssh_private_key`
PEM string of the private key used by the log viewer to SSH into managed servers.

```js
db.settings.insertOne({
  name: "hetzner_ssh_private_key",
  value: "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----"
})
```

> Hetzner billing is per hour. `deletionWindowMinutes: 5` means servers are deleted
> at minute 55–59 of each billing hour (when idle and no active jobs).

### `advert_vdo` item shape
```json
{
  "id": "abc",
  "name": "Ad Name",
  "mp4Url": "https://...",
  "websiteUrl": "https://...",
  "skipSeconds": 5,
  "isActive": true
}
```

### `advert_image` shape
```json
{
  "isActive": true,
  "imageUrl": "https://...",
  "websiteUrl": "https://...",
  "showOn": ["ready", "end", "pause"]
}
```

### `advert_javascript`
Plain HTML string (script tag).

---

## Ad Resolution Logic

| Space Plan | Video Ads | Image Ad | JS Ad |
|---|---|---|---|
| `free` (default) | VAST always enabled → `/vast.xml` | from `advert_image` setting | from `advert_javascript` setting |
| `paid` | from `CustomDomain.Advert[]` | from `CustomDomain.AdvertImage` | from `CustomDomain.AdvertJavascript` |

---

## Cache Headers

| Response | Cache-Control | CDN-Cache-Control |
|---|---|---|
| Embed player | `no-store` | `max-age=14400` (4h) |
| Maintenance / Error / Processing | `no-store` | `max-age=60` (1m) |
| JS / CSS static files | `no-store` | `max-age=2592000` (30d) |

---

## API Routes

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check — `{"status":"ok","service":"server-service"}` |
| `GET` | `/logs` | List log files (JSON) |
| `GET` | `/logs/{filename}?tail=200` | Read log file, newest lines first |
| `GET` | `/ui` | Web UI — real-time log viewer + Hetzner dashboard |
| `GET` | `/ws` | WebSocket — real-time log streaming |
| `GET` | `/hetzner/servers` | List managed Hetzner servers (JSON) |
| `GET` | `/hetzner/log?ip=x.x.x.x` | Fetch cloud-init log via SSH |

---

## Web UI (`/ui`)

Two-tab dashboard:

**📋 Logs** — Real-time log streaming via WebSocket. Select log file from sidebar.

**🖥️ Hetzner** — Live server dashboard:
- Server status (`running` / `initializing` / `off`)
- Public IP address
- **📋 View Install Log** — SSH into the server and display `/var/log/cloud-init-output.log`
- **🔑 Copy SSH** — Copy `ssh root@{ip}` to clipboard

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-service/main/install.sh | sudo bash -s -- \
    --port 8084 \
    --mongodb-uri "mongodb+srv://user:pass@host/platform"
```

### Update binary only (preserve `.env`)

```bash
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-service/main/install.sh | sudo bash -s -- \
    --port 8084
```

### Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-service/main/install.sh | sudo bash -s -- --uninstall
```

---

## Service Management

```bash
systemctl status  server-service
systemctl restart server-service
systemctl stop    server-service
journalctl -u server-service -f
```

---

## Release

```bash
git tag v1.0.0
git push origin v1.0.0
```
