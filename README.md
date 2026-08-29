# Lambs Server (Go)

Universal project management backend. One binary manages all projects.

## Quick Deploy

```bash
# Build
cd go-server && GOOS=linux GOARCH=amd64 go build -o lambs-server .

# Deploy
scp lambs-server ubuntu@YOUR_SERVER:~/apps/lambs-server/
ssh ubuntu@YOUR_SERVER \
  "sudo systemctl stop lambs-server && \
   sudo cp ~/apps/lambs-server/lambs-server /usr/local/bin/ && \
   sudo systemctl start lambs-server"
```

See `deploy/deploy.sh` for full frontend+backend deployment.

## Environment

| Variable | Required | Default | Notes |
|----------|----------|---------|-------|
| `DATABASE_URL` | Yes | — | PostgreSQL DSN. Accepts `postgresql+asyncpg://` prefix |
| `JWT_SECRET` | Yes | — | 64 hex chars recommended |
| `PORT` | No | 3602 | HTTP listen port |
| `LAMBS_CONFIG_PATH` | No | `/home/ubuntu/apps/lambs-server/lambs_config.json` | |

Store secrets in `/home/ubuntu/apps/lambs-server/.env` (chmod 600). See `deploy/.env.example`.

## Architecture

```
┌─────────────────────────────────────────────┐
│ Web host                                    │
│  nginx → /Lambs/ → lambs-server:3602       │
│        → /<project>/ → gate → backend       │
│        → /tg-webhook → tg-bot               │
├─────────────────────────────────────────────┤
│ App host                                    │
│  lambs-server (Go)                          │
│  tg-bot (Go, 内嵌 webhook server)            │
│  PostgreSQL 16                              │
│  Redis                                      │
└─────────────────────────────────────────────┘
```

## API Reference

### Auth
| Method | Path | Auth | Notes |
|--------|------|------|-------|
| POST | `/api/auth/login` | No | Returns JWT |
| GET | `/api/auth/me` | JWT | Current user |

### Projects
| Method | Path | Auth | Notes |
|--------|------|------|-------|
| GET | `/api/projects` | JWT | List all |
| POST | `/api/projects` | JWT | Create |
| GET | `/api/projects/{id}` | JWT | Detail |
| PUT | `/api/projects/{id}` | JWT | Update |
| DELETE | `/api/projects/{id}` | SuperAdmin | Delete |
| PATCH | `/api/projects/{id}/status` | JWT | Toggle online/offline/maintenance |
| POST | `/api/projects/{id}/clone` | JWT | Clone project |
| POST | `/api/projects/{id}/sync` | JWT | Sync user count |
| POST | `/api/projects/refresh-all` | JWT | Sync all |

### Runtime (Unified Process Manager)
| Method | Path | Auth | Notes |
|--------|------|------|-------|
| POST | `/api/runtime/ports/allocate/{id}` | SuperAdmin | Auto-allocate port (3510-3599) |
| POST | `/api/runtime/proc/start/{id}` | SuperAdmin | Start managed process |
| POST | `/api/runtime/proc/stop/{id}` | SuperAdmin | Stop managed process |
| POST | `/api/runtime/proc/restart/{id}` | SuperAdmin | Restart |
| GET | `/api/runtime/proc/status/{id}` | JWT | Process status |
| GET | `/api/runtime/proc/list` | SuperAdmin | All managed processes |

### Backups
| Method | Path | Auth | Notes |
|--------|------|------|-------|
| POST | `/api/backups/{id}` | JWT | Create backup |
| GET | `/api/backups/{id}` | JWT | List backups |
| POST | `/api/backups/{id}/upload-tg/{file}` | JWT | Upload to Telegram (GPG encrypted) |
| GET | `/api/backups/{id}/download/{file}` | Admin | Download |
| POST | `/api/backups/{id}/restore/{file}` | Admin | Restore SQLite |

### Users, Settings, Notifications
| Method | Path | Auth | Notes |
|--------|------|------|-------|
| GET | `/api/users` | SuperAdmin | List users |
| POST | `/api/users` | SuperAdmin | Create user |
| PUT | `/api/users/{id}` | SuperAdmin | Update user |
| DELETE | `/api/users/{id}` | SuperAdmin | Delete |
| POST | `/api/users/{id}/reset-password` | SuperAdmin | Reset password |
| GET | `/api/settings/config` | SuperAdmin | Get config |
| PUT | `/api/settings/config` | SuperAdmin | Update config |
| GET | `/api/settings/export/users` | SuperAdmin | CSV export |
| GET | `/api/settings/export/projects` | SuperAdmin | CSV export |
| GET | `/api/settings/export/project-users/{id}` | SuperAdmin | CSV export |
| GET | `/api/notifications` | JWT | List notifications |
| POST | `/api/notifications/{id}/read` | JWT | Mark read |
| POST | `/api/notifications/read-all` | JWT | Mark all read |
| GET | `/api/system/health` | JWT | CPU/Mem/Disk/Uptime |
| GET | `/api/health` | No | Simple health check |

## Runtime Management

### Enable
Set `"runtime_enabled": true` in `lambs_config.json`.

### How It Works
1. Create project with `service_name` field set → Lambs auto-allocates port
2. Change project status → Lambs auto-starts/stops the process
3. Health monitor checks every 30s → auto-restarts crashed processes
4. Delete project → port freed, process stopped

### Binary Convention
Managed project binaries must exist at:
```
/home/ubuntu/apps/<service_name>/<service_name>
```
The `PORT` env var is passed so the child binds the allocated port.

### Memory Model
| Component | Memory | Notes |
|-----------|--------|-------|
| lambs-server | 2MB | Full HTTP+DB+business logic |
| tg-bot (Go) | 9MB | Polling bot, 10 commands |
| tg-webhook (Web1) | 10MB | Wake-up endpoint |
| Managed project proxy | 3MB | Per idle project |

## TG Backup Storage

Backups auto-encrypted with GPG (AES256) before upload to Telegram.
Requires `/opt/wool-tools/.tg-secrets` with:
- `TG_BOT_TOKEN` — Bot API token
- `TG_CHANNEL_BACKUP` — Channel ID for backups
- `GPG_PASS` — GPG symmetric passphrase

Upload via API or auto-upload after backup creation.

## tg-bot (Go Rewrite)

Separate binary in `cmd/tg-bot/`. Replaces Python tg-bot.py (30MB → 9MB).
Same 10 commands, same polling→webhook idle pattern.

Build: `cd cmd/tg-bot && GOOS=linux GOARCH=amd64 go build -o tg-bot .`

## First Run (fresh database)

Core tables (`users`, `projects`, `notifications`, `audit_logs`) are created
**automatically at startup** — no manual SQL needed. The first registered
account becomes `super_admin`. After bootstrapping, set
`LAMBS_ALLOW_REGISTER=false` in `.env` so nobody can race to grab the first
account on a public deployment.

## What It Manages

- **Projects** — status machine (在线/离线/维护中), process start/stop via
  `startup_command` (supports `cd /dir && cmd`) or a systemd unit
  (`service_name`), auto-restart with 30s health checks, TCP proxy, scheduled
  tasks (cron, dual-channel: Linux + Windows agent)
- **Data sources** — 8 types, all real-machine verified:
  PostgreSQL / MySQL / MSSQL / MongoDB / Redis / SQLite / REST (HTTP
  convention) / Qdrant (vector). Table browse + CRUD + backups per type.
  DSN scheme whitelist is validated at save time; connectivity via
  测试连接 (test connection).
- **Users & RBAC** — super_admin / project_admin / viewer, project-level
  grants, salted passwords (R7 contract), password reset (super_admin any
  user, project_admin shared-project users only)
- **Backups** — GPG-encrypted (AES256) upload to Telegram, list/restore/
  download
- **Observability** — audit log, notification center, multi-node system
  monitor (本机 + wool + windows-agent), aggregated logs
- **Gate** — nginx auth_request offline/maintenance page with project
  branding

## Database Notes

- PostgreSQL is the only required store (system metadata). Project data
  sources are separate and connect out from the server.
- The server opens `LAMBS_DB_MAX_CONNS` (default 30) pooled connections;
  tune per deployment size.

## License

MIT
