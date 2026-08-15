# Lambs管理系统

> 版本 V2.0.0 · 2026-08-14 · 通用项目管理后台
>
> 架构：React SPA (`web/`) + Go 单二进制 (`go-server/`) + PostgreSQL
>
> 访问：https://wool.cc.cd/lambs/

---

## 一、项目简介

Lambs 是一个**通用项目管理后台**：一个仪表盘管理所有项目——不论语言、框架、数据库。

**核心设计：协议优于侵入** — 被管项目只填连接串，不装 Agent。Lambs 通过标准协议连接。

### 能力总览（V2.0.0）

| 能力 | 说明 |
|------|------|
| 7 种数据源 | PostgreSQL / SQLite / MySQL / MongoDB / Redis / REST API / **向量数据库（Qdrant）** |
| 多数据源 | 一个项目可挂多个数据源，各自独立浏览、独立写 |
| 共享服务按需 | 引用计数注册表：多项目同名服务共享实例，最后停用才停 |
| 进程管理 | 项目专属进程启停、开机错峰拉起、30s 漂移自愈、内存门控 |
| 按需代理 | TCPProxy：端口常驻、进程按需唤醒、空闲回收 |
| 访问闸门 | 项目三态（在线/离线/维护）→ nginx 联动 + 品牌化拦截页 |
| 数据浏览 | 表格 / DocView（Mongo 树）/ KVView（Redis 五类型）/ VectorView（向量检索） |
| 开放注册 | 注册即 viewer，超管授权后可见项目 |
| 审计 | 登录/注册/行 CRUD/项目与用户管理全落库，聚合日志流展示 |
| 备份 | SQLite/PG 备份、WAL 安全恢复、GPG 加密传 TG、定时+保留策略 |
| 权限 | RBAC 三角色 + 项目级授权，非超管 DSN/数据源自动脱敏 |
| 体验 | 12 套主题、全站圆角下拉、分节表单、日志时间线、弱网重试 |

---

## 二、系统架构

```
用户浏览器
   ↓ Cloudflare
Web1 (nginx + 静态前端)
   ├─ /lambs/           前端 SPA
   ├─ /lambs/api/       → App1:3602 (lambs-server)
   └─ /<项目路径>/*      auth_request 闸门 → App1 TCPProxy:端口
                              ↓
App1 (lambs-server 单二进制)
   ├─ PostgreSQL :5433     Lambs 主库
   ├─ ProcMgr              专属进程 + 共享服务注册表
   ├─ TCPProxy :3510-3599  按需代理
   └─ 共享服务              redis-server / qdrant …（按需启停）
```

**服务归属规则：**
- `startup_command` → 项目专属进程，项目启停即启停
- `services[].name` → 共享实例，引用计数（0→1 启动，1→0 停止）
- 只填连接串 → 纯数据源，不碰进程

---

## 三、项目结构

```
Lambs管理系统/
├── go-server/          Go 后端（单二进制 ~13MB）
│   ├── main.go         路由 + 健康 + 清理 + 检测端点
│   ├── internal/
│   │   ├── auth/       JWT + bcrypt + 忘记密码验证码
│   │   ├── db/         7 种数据源 adapter（工厂模式）
│   │   ├── handlers/   项目/用户/通知/设置/备份/检索
│   │   ├── runtime/    ProcMgr 注册表 + PortMgr + TCPProxy
│   │   ├── nginx/      被管配置生成 + 同步推送
│   │   ├── gate/       闸门检查 + 品牌化拦截页
│   │   ├── notify/     SMTP
│   │   └── tgbackup/   TG 备份通道（GPG）
│   └── cmd/tg-bot/     TG 管理机器人
├── web/                React SPA（Vite）
│   └── src/
│       ├── pages/      6 页面 + 项目详情
│       ├── components/ ProjectForm / DocView / KVView / VectorView …
│       └── api/        client（超时+重试）
├── server/             【历史】Python 版，已退役，仅参考
└── 服务器配置/          nginx 配置、OCI 文档、脚本
```

---

## 四、数据模型

### projects
| 字段 | 类型 | 说明 |
|------|------|------|
| id | VARCHAR | 仓库名生成 |
| datasources | JSONB | `[{id,name,type,dsn,is_primary}]` 主源镜像 dsn/db_type |
| services | JSONB | `[{name,start_cmd,stop_cmd}]` 共享服务引用 |
| tags / features / tabs | JSONB | 标签 / 统计卡 / 表快照 |
| status | online/offline/maintenance | 三态循环 |
| port / base_path | | 运行时端口 / nginx 路径 |
| startup_command | | 专属进程启动命令 |
| backup_interval_hours / retention_days | | 备份策略 |

### users / notifications / audit_logs / verification_codes

RBAC：super_admin / project_admin / viewer，project_access 控制项目级可见性。

---

## 五、API 端点（核心）

| 域 | 端点 |
|----|------|
| 认证 | POST /api/auth/login · forgot-password/request|verify |
| 项目 | GET/POST /api/projects · PUT/DELETE /{id} · PATCH status|pin · reorder · clone |
| 数据 | GET /{id}/tables/list?ds= · tables?table&ds= · 行 CRUD data/row |
| 检索 | POST /{id}/vector-search |
| 运行时 | proc/start|stop|status · proxy/start|stop · ports/allocate · detect · local-services |
| 备份 | POST/GET /api/backups/{id} · download · restore · upload-tg |
| 设置 | config · export/* · audit-logs · datasources |
| 闸门 | GET /api/gate/check · check-internal（nginx auth_request）· offline-page |

---

## 六、运维手册

### 部署舞步（后端）
```
build(Windows, GOOS=linux) → scp → systemctl stop → fuser -k 3602/tcp
→ cp 到 /usr/local/bin → chmod 755 → start → curl health
```

### 前端
```
npm run build → tar → Web1 sudo 解包到 /var/www/lambs（root 拷贝，www-data 属主）
```

### 测试
```
go-server: go test ./...（Windows 可跑，平台层已拆分）
生产验证: 全部 API + 浏览器真机，零 mock（7 种数据源真库）
```

### 关键配置
- App1: lambs_config.json（runtime_enabled=true 自愈开）、.env（DATABASE_URL/JWT_SECRET）
- 共享服务单元必须 disabled（防重启常驻），由 Lambs 按计数拉
- LAMBS_MIN_FREE_MB 可调内存门控阈值（默认 100MB）
- nginx: lambs_login 5r/m 登录限流、lambs-managed.conf 自动生成

---

## 七、版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| V1.0.0 | 2026-07-26 | Python 版（FastAPI+React），6 页面 30+ 端点 |
| V2.0.0 | 2026-08-14 | Go 重写 + 多数据源（方案 B）+ 共享服务注册表 + 向量库 + 全链路真机验证 |
| V2.0.1 | 2026-08-15 | 全量深度验收：3 CRITICAL（SQL 注入/日志越权/nginx 注入）实锤修复，25 项缺陷清零，安全规范 8 条补强，双端生产复验 33/0 |
