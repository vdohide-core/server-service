# server-service

Background job orchestrator and infrastructure management service for vdohide-core.  
Runs scheduled tasks including file cleanup, domain verification, storage sync, S3 cleanup, **Hetzner Auto-Scaler**, and **Worker Heartbeat Cleanup**.  
Provides a Web UI dashboard for monitoring logs, workers, and Hetzner servers.

---

## Features

### Infrastructure
- Settings sync every 1 minute from MongoDB → `conf/setting.json`
- Domain cache sync → `conf/domains.json`
- Space cache sync → `conf/spaces.json`
- Rotating log file (25 MB per file, startup rotation)
- Log reader API (`GET /logs`, `GET /logs/{filename}?tail=200`)
- **Web UI** (`/ui`) — real-time log viewer + Worker dashboard + Hetzner server dashboard

### Schedulers (every 1 minute)

| Scheduler | Description |
|---|---|
| Space capacity sync | Calculate storage usage per space (batch=5) |
| Domain CNAME verify | Verify pending custom domains (batch=5) |
| File cleanup | Hard-delete soft-deleted files (batch=5) |
| Space cleanup | Mark files in deleted spaces for deletion |
| S3 storage cleanup | Purge orphaned media/ingest objects from S3 |
| Hetzner auto-scaler | Spin up/down download servers based on pending jobs |
| Worker cleanup | Mark stale workers offline (3 min), delete old records (1 hour) |
| Original cleanup | Soft-delete original media after file transcoded to highest resolution |

> All schedulers can be disabled with `SCHEDULERS=false` for dev/test (HTTP-only mode).

### Hetzner Auto-Scaler
- Runs every 1 minute, config loaded from MongoDB `hetzner_auto_scale` setting
- Counts pending download jobs from `files` collection:
  - `status: "waiting"` + `clonedFrom` not set + not trashed/deleted
  - No existing `medias` or `video_process` records
  - Has `ingest` record **or** `metadata.source` set
- **Weighted scaling**: jobs are classified as `slow` (missav HLS) or `fast` (upload/direct/others)
  - `slowPerServer` — missav jobs per server (default 2)
  - `fastPerServer` — upload/direct/others per server (default 5)
  - Falls back to `downloadsPerServer` if weighted fields not set
- Scales UP: `ceil(weightedPending)` servers, max `maxServers`
  - Skips creation if any server is still `initializing`
  - Server name: `vdohide-dl-{timestamp}-{i}` (unique per batch)
  - SSH keys injected at creation for remote access
- Scales DOWN: deletes servers at billing hour boundary (minute 55–59) when idle
  - Each server checked individually — busy servers are skipped
  - Waits for next billing hour if still processing
  - Checks `video_process.workerId` matching `{serverName}@N` before deletion
- Cloud-Init: runs install script via `user_data` on boot

### Worker Heartbeat System
- Download workers (`server-download`) upsert heartbeat to `workers` collection every 1 minute
- Worker cleanup scheduler marks workers as `offline` after 3 minutes without heartbeat
- Workers offline for more than 1 hour are automatically deleted
- Each worker reports: `workerId`, `hostname`, `ip`, `pid`, `status` (idle/busy/offline), `enable`, `activeJobs`, `maxJobs`, `system` (disk/mem/CPU)
- `enable` flag allows pausing a worker from accepting new jobs

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8081` | HTTP listen port |
| `MONGODB_URI` | _(required)_ | MongoDB connection string (also reads `MONGO_URI`, `DATABASE_URL`) |
| `LOG_PATH` | `logs/server-service.log` | Log file path |
| `STORAGE_ID` | _(empty)_ | Default storage ID |
| `STORAGE_PATH` | `./files` | Local storage path |
| `SCRAPER_URL` | _(empty)_ | URL scraper endpoint |
| `HTTP_TIMEOUT` | `30` | HTTP request timeout (seconds) |
| `SCHEDULERS` | `true` | Set to `false` to disable all background schedulers |

---

## Settings (MongoDB `settings` collection)

| Name | Type | Description |
|---|---|---|
| `download_enabled` | `boolean` | Enable/disable download processing |
| `url_scraping` | `string` | URL scraping configuration |
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
  "slowPerServer": 2,
  "fastPerServer": 5,
  "idleMinutes": 10,
  "deletionWindowMinutes": 5,
  "mongoUri": "mongodb+srv://...",
  "storagePath": "/home/files",
  "installUrl": "https://raw.githubusercontent.com/vdohide-core/server-download/main/install.sh",
  "sshKeys": ["rsa-key-20230228"],
  "sshKeyContent": "...",
  "sshKeyPath": "..."
}
```

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable/disable the scaler |
| `apiToken` | _(required)_ | Hetzner Cloud API token |
| `serverType` | `cx22` | Hetzner server type |
| `location` | `nbg1` | Hetzner datacenter location |
| `maxServers` | `10` | Maximum concurrent servers |
| `downloadsPerServer` | `2` | Concurrent downloads per server (legacy fallback) |
| `slowPerServer` | `2` | Missav HLS jobs per server (heavy, weighted) |
| `fastPerServer` | `5` | Upload/direct/other jobs per server (light, weighted) |
| `idleMinutes` | `10` | Minimum idle time before checking billing boundary |
| `deletionWindowMinutes` | `5` | Minutes before billing hour end to delete (e.g. 5 = delete at minute 55–59) |
| `mongoUri` | _(required)_ | MongoDB URI passed to install script |
| `storagePath` | `/home/files` | Storage path passed to install script |
| `installUrl` | _(GitHub raw URL)_ | Install script URL |
| `sshKeys` | `[]` | Hetzner SSH key names/IDs to add to new servers |
| `sshKeyContent` | _(empty)_ | SSH key content string |
| `sshKeyPath` | _(empty)_ | Path to SSH key file |

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

---

## API Routes

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check — `{"status":"ok","service":"server-service"}` |
| `GET` | `/logs` | List log files (JSON) |
| `GET` | `/logs/{filename}?tail=200` | Read log file, newest lines first |
| `GET` | `/ui` | Web UI — real-time log viewer + Worker + Hetzner dashboard |
| `GET` | `/ws` | WebSocket — real-time log streaming |
| `GET` | `/workers` | List all workers with `isOnline` flag (JSON) |
| `GET` | `/hetzner/servers` | List managed Hetzner servers (JSON) |
| `GET` | `/hetzner/log?ip=x.x.x.x` | Fetch cloud-init log via SSH |

---

## Web UI (`/ui`)

Three-tab dashboard:

**📋 Logs** — Real-time log streaming via WebSocket. Select log file from sidebar.

**👷 Workers** — Worker heartbeat dashboard:
- Summary stats: Total / Online / Busy / Idle / Offline
- Worker cards with status (🟢 idle / 🟡 busy / 🔴 offline)
- Enable/Disable tag per worker
- System metrics: Disk and Memory usage with progress bars
- Host, IP, PID, active jobs, heartbeat timestamp

**🖥️ Hetzner** — Live server dashboard:
- Server status (`running` / `initializing` / `off`)
- Public IP address
- **📋 View Install Log** — SSH into the server and display `/var/log/cloud-init-output.log`
- **🔑 Copy SSH** — Copy `ssh root@{ip}` to clipboard

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-service/main/install.sh | sudo bash -s -- \
    --port 8081 \
    --mongodb-uri "mongodb+srv://user:pass@host/platform"
```

### Update binary only (preserve `.env`)

```bash
curl -fsSL https://raw.githubusercontent.com/vdohide-core/server-service/main/install.sh | sudo bash -s -- \
    --port 8081
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
