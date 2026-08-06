# Configuration Reference · 配置参考

Everything both services read, write, and listen on. / 两个服务的全部输入输出。

## 1. Components · 组件

| Component | Binary | Default port | Runs as |
|---|---|---|---|
| engram MCP server (patched) | `/usr/local/bin/engram` | `7440` | system user `engram` |
| Admin panel | `/usr/local/bin/engram-admin` | `7441` | system user `engramadm` (member of group `engram`) |

## 2. engram server (patch v2) · 服务端环境变量

| Variable | Required | Meaning |
|---|---|---|
| `ENGRAM_MCP_HTTP` / `--http <addr>` | yes (for LAN mode) | Listen address, e.g. `0.0.0.0:7440`. Without it the binary is 100% upstream stdio behavior. |
| `ENGRAM_MCP_TOKENS_FILE` | one of the two | Path to the token list JSON. Enables multi-account auth with hot reload. |
| `ENGRAM_MCP_TOKEN` | one of the two | Single shared token (legacy fallback, used only when the list file var is unset). |
| `ENGRAM_PROJECT` / `--project <name>` | recommended | Default project for writes that do not specify one. |
| `ENGRAM_DATA_DIR` | yes | Data directory containing `engram.db`. |

Endpoints: `/mcp` (auth required), `/healthz` (open).

### Token list file format

```json
{
  "alice": {
    "token": "eng_X7k…",
    "revoked": false,
    "created_at": "2026-08-06T07:59:07Z",
    "note": "backend team"
  }
}
```

- The server reads only `token` and `revoked`; other fields belong to the panel.
- The file is reloaded automatically when its mtime/size changes — issue/revoke needs **no restart**.
- Missing or invalid file → **fail-closed**: every request gets 401 until the file is readable again.

## 3. Admin panel · 面板环境变量

| Variable | Default | Meaning |
|---|---|---|
| `ENGRAM_ADMIN_ADDR` | `:7441` | Listen address. |
| `ENGRAM_DB` | `/var/lib/engram-mcp/.engram/engram.db` | engram SQLite file, opened strictly `mode=ro`. |
| `ENGRAM_TOKENS_FILE` | `/etc/engram-mcp/tokens.json` | Token list the panel manages (the **only** file it may write). |
| `ENGRAM_ADMIN_PASS_BCRYPT` | — | bcrypt hash of the admin password. Generate with `engram-admin hashpw` (password on stdin). |
| `ENGRAM_ADMIN_PASSWORD` | — | Plaintext password, **dev only**. Ignored when the bcrypt var is set. |
| `ENGRAM_ADMIN_SESSION_TTL` | `12h` | Login session lifetime (in-memory sessions; restart = re-login). |
| `ENGRAM_ADMIN_SUBTITLE` | `LAN shared memory hub` | Login page subtitle. |
| `ENGRAM_ADMIN_MCP_URL` | `http://127.0.0.1:7440/mcp` | MCP URL shown in the client onboarding hint and in each member's copy-ready `claude mcp add` command. |
| `ENGRAM_ADMIN_DB` | `/etc/engram-mcp/admin.db` | Panel-owned SQLite for the account system (users, roles, bcrypt hashes). The directory must be writable by the panel user. |

### Accounts & roles · 账号与角色

- On first start with an empty `ENGRAM_ADMIN_DB`, the panel seeds one **admin** account: username `admin`, password from `ENGRAM_ADMIN_PASS_BCRYPT` (or `ENGRAM_ADMIN_PASSWORD` in dev).
- Admin creates member accounts in the **用户管理 / Users** page. Members log in with username + password and use **我的 Key / My Key** to mint or regenerate their own agent token (name = username, written to the same token list file, hot-reloaded by the server).
- Deleting a user also deletes their token; disabling a user kills their sessions immediately. Admin cannot disable/delete itself.
- All `/api/users*` and `/api/tokens*` endpoints require role=admin (403 for members). `/api/me*` requires any valid session.

Endpoints: `GET /` (UI), `POST /api/login`, `POST /api/logout`, `GET /api/meta`, `GET /healthz` — open;
everything under `/api/stats/*`, `/api/memories*`, `/api/me*`, `/api/tokens*`, `/api/users*` — session required.

### Panel API summary

| Method & path | Purpose |
|---|---|
| `GET /api/stats/overview` | Totals: memories, sessions, projects, prompts, pinned, duplicates intercepted, DB size, last write |
| `GET /api/stats/timeseries?days=N` | Per-day write counts (N ≤ 366, gaps zero-filled) |
| `GET /api/stats/breakdown` | Counts by type / project / scope |
| `GET /api/stats/topics?limit=N` | topic_key ranking by revision count |
| `GET /api/memories?q=&project=&type=&page=&size=` | Browse or FTS5 search (BM25), paginated |
| `GET /api/memories/{id}` | Full record incl. content |
| `GET /api/me` | Current account: role, dates, own full token (owner-only by design) |
| `POST /api/me/password` | Change own password `{old_password, new_password}` |
| `POST /api/me/token` | Mint or regenerate own agent token → returns full token **exactly once per call** |
| `GET /api/users` | (admin) List accounts |
| `POST /api/users` | (admin) Create `{username, password}` |
| `POST /api/users/{name}/disable` · `enable` | (admin) Disable / re-enable (kills sessions immediately) |
| `POST /api/users/{name}/reset-password` | (admin) Set a new password |
| `DELETE /api/users/{name}` | (admin) Delete account **and** its agent token |
| `GET /api/tokens` | (admin) List tokens (suffix only — full token is never returned after creation) |
| `POST /api/tokens` | (admin) Issue `{name, note}` → returns the full token **exactly once** |
| `POST /api/tokens/{name}/revoke` · `unrevoke` | (admin) Disable / re-enable |
| `PATCH /api/tokens/{name}` | (admin) Update note |
| `DELETE /api/tokens/{name}` | (admin) Remove permanently |

## 4. Filesystem layout · 文件与权限

| Path | Owner | Mode | Purpose |
|---|---|---|---|
| `/var/lib/engram-mcp` | `engram:engram` | `750` | Parent of data dir (must be traversable by group!) |
| `/var/lib/engram-mcp/.engram/` | `engram:engram` | `750` | Data dir |
| `…/.engram/engram.db*` | `engram:engram` | `640` | DB + WAL/SHM; group read = panel's only access |
| `/etc/engram-mcp` | `engramadm:engram` | `750` | Config dir (panel needs write for atomic token writes) |
| `/etc/engram-mcp/tokens.json` | `engramadm:engram` | `660` | Token list: panel writes, server reads via group |
| `/etc/engram-mcp/admin.db*` | `engramadm:engramadm` | `600` | Panel account database (users/roles/bcrypt), created on first run |
| `/etc/engram-mcp/admin.env` | `engramadm:engramadm` | `600` | `ENGRAM_ADMIN_PASS_BCRYPT=…` |
| `/etc/engram-mcp.env` | `root:root` | `600` | Server env (EnvironmentFile of engram-mcp.service) |

> ⚠️ Two easy-to-miss details: the **parent** `/var/lib/engram-mcp` also needs group-traverse permission
> (relaxing only the inner dir is not enough), and `/etc/engram-mcp` must be owned by the panel user or
> atomic tmp+rename writes fail with EACCES.

## 5. systemd units · 示例

`docs/engram-mcp.service.example`:

```ini
[Unit]
Description=engram LAN MCP server (patched, token-list auth)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=engram
Group=engram
EnvironmentFile=/etc/engram-mcp.env
ExecStart=/usr/local/bin/engram mcp --http 0.0.0.0:7440 --project=team-kb
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

`docs/engram-admin.service.example`:

```ini
[Unit]
Description=engram admin panel (token management + read-only memory view)
After=network-online.target engram-mcp.service
Wants=network-online.target

[Service]
Type=simple
User=engramadm
Group=engram
Environment=ENGRAM_ADMIN_ADDR=:7441
Environment=ENGRAM_DB=/var/lib/engram-mcp/.engram/engram.db
Environment=ENGRAM_TOKENS_FILE=/etc/engram-mcp/tokens.json
EnvironmentFile=/etc/engram-mcp/admin.env
ExecStart=/usr/local/bin/engram-admin
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/etc/engram-mcp
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

`/etc/engram-mcp.env` example:

```dotenv
ENGRAM_DATA_DIR=/var/lib/engram-mcp/.engram
ENGRAM_MCP_TOKENS_FILE=/etc/engram-mcp/tokens.json
# legacy single-token fallback, only used when TOKENS_FILE is unset:
# ENGRAM_MCP_TOKEN=…
```

## 6. Client onboarding · 同事接入

The panel's **我的 Key / My Key** page generates every snippet below with the member's token already filled in, plus a copy button — members never hand-edit a token. All of them are **write-once, permanent**: re-config only after regenerating the key, revocation, or a new machine. / 面板「我的 Key」页已把下列配置按成员 token 填好，一键复制。全部一次配置永久生效，只有重生 key / 被吊销 / 换电脑才需要重配。

### Claude Code

```bash
# global, all projects (recommended) · 全局，所有项目生效
claude mcp add --transport http engram-team http://<server>:7440/mcp \
  --header "Authorization: Bearer <token>" -s user        # writes ~/.claude.json

# this project only, shareable via git · 仅本项目，.mcp.json 可提交共享
claude mcp add --transport http engram-team http://<server>:7440/mcp \
  --header "Authorization: Bearer <token>" -s project     # writes ./.mcp.json

# verify
claude mcp list   # expect: engram-team … ✔ Connected
```

### Codex — append to `~/.codex/config.toml`

```toml
[mcp_servers.engram-team]
url = "http://<server>:7440/mcp"
http_headers = { Authorization = "Bearer <token>" }
```

### Cursor — merge into `~/.cursor/mcp.json`

```json
{
  "mcpServers": {
    "engram-team": {
      "url": "http://<server>:7440/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

### Gemini CLI — merge into `~/.gemini/settings.json`

```json
{
  "mcpServers": {
    "engram-team": {
      "httpUrl": "http://<server>:7440/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

Other agents: use their HTTP-transport MCP config with the same URL and `Authorization: Bearer <token>` header.

## 7. Uninstall · 完全卸载

```bash
sudo systemctl disable --now engram-mcp engram-admin
sudo rm -f /etc/systemd/system/engram-{mcp,admin}.service \
           /usr/local/bin/{engram,engram-admin} /etc/engram-mcp.env
sudo rm -rf /etc/engram-mcp /var/lib/engram-mcp      # deletes the memory base!
sudo userdel engram && sudo userdel engramadm
```
