# Lambs管理系统 · 需求文档

> 版本 V2.0.0 · 2026-08-14
>
> 后端 `go-server/`（Go 单二进制）+ 前端 `web/`（React SPA）
>
> 本文档描述 V2.0.0 的功能需求与设计规范。

---

## 一、产品定位

通用项目管理后台。**协议优于侵入**：被管项目只提供连接串/API 地址，不安装 Agent。

## 二、功能需求

### 2.1 数据源（7 种连接方式）

| 类型 | 连接串示例 | 能力 |
|------|-----------|------|
| 直连 PostgreSQL | postgresql://u:p@h:5432/db | 表/行 CRUD、用户同步 |
| 直连 SQLite | sqlite:////path/db | 表/行 CRUD、备份/恢复 |
| MySQL | mysql://u:p@h:3306/db | 表/行 CRUD、用户同步、中文 |
| MongoDB | mongodb://h:27017/db | 集合文档树 CRUD |
| Redis | redis://:p@h:6379/0 | 五类型 KV 视图、键 CRUD |
| REST API | http(s)://h/api | 约定式 CRUD |
| 向量数据库（Qdrant） | qdrant://h:6333 | 集合/点 CRUD、相似度检索 |

**多数据源**：单项目可挂多个源（datasources 数组），详情页切换浏览，主源驱动连接测试/同步/备份。

### 2.2 共享服务按需

- services 数组声明 `{name, start_cmd, stop_cmd}`
- 同名服务多项目共享实例，引用计数：0→1 启动、1→0 停止
- 上线顺序：共享服务 → TCPProxy → 专属进程；停止反序
- 自愈：refs>0 但进程不在 → 30s 内自动拉起 + 通知
- 内存门控：可用内存 < 阈值（默认 100MB，LAMBS_MIN_FREE_MB 可调）拒绝启动
- 开机：逐项目 2s 错峰；共享服务只启动一次
- 系统依赖保护：local-services 端点标记 postgresql/lambs-server 等不可按需管理

### 2.3 项目生命周期

- 三态循环：在线 → 离线 → 维护 → 在线
- 闸门：Web1 nginx auth_request → App1 gate；拦截页品牌化（项目 logo + 主题色 + 离线文案）
- 删除：先停进程/解挂服务再删行（幽灵引用已防）
- 克隆：仅元数据 + 连接串（不复制端口/路径/进程配置）

### 2.4 认证与权限

- JWT（8h）+ bcrypt(sha256(pwd)) 双层
- **开放注册**：注册即 viewer + 空 project_access，超管授权后才能看项目；注册即自动登录
- 忘记密码：用户名+邮箱验证 → 6 位码邮件（60s 冷却 / 5 分钟过期 / 一次性 / 每码最多 5 次尝试）
- **改密即时失效**：users.token_version 版本号入 JWT，改密/重置/忘记密码 +1，旧 token 立即 401；自己改密走 PUT /api/auth/me/password（旧密码校验）
- RBAC：super_admin / project_admin / viewer + project_access 项目级授权
- 非超管：DSN / datasources / services 一律脱敏，写路径按角色拦截
- 登录限流：nginx lambs_login 5r/m + Go 层内存限流 5r/min/IP（直连 :3602 双保险）
- **XFF 信任链**：仅信任 LAMBS_TRUSTED_PROXIES 列表（部署 env，默认本机），其余来源忽略 XFF 防轮换绕过
- 角色/停号变更即时生效（每请求查库，非等 token 过期）

### 2.5 备份

- SQLite 备份 API（WAL 安全）、PG 逻辑备份
- 恢复：sqlite3 备份 API 原子写回（不直接覆盖源文件）
- 定时备份 + 保留天数清理 + GPG 加密自动传 TG
- 通知/审计自动清理：30 天 / 90 天，每小时

### 2.6 前端体验

- 弱网：15s 请求超时 + GET 瞬时失败（网络异常/502/503/504）重试一次；写请求不重试
- 响应式：375px 断点侧边栏折叠、移动端可用
- 表单自动化：端口留空自动分配、base_path 默认 /仓库名、127.0.0.1 连接串自动识别共享服务、启动命令「检测」回填
- **表单设计**：分节卡片（基本信息/数据源/访问与进程/备份）+ 高级折叠（共享服务）+ sticky 操作条；数据源行两行式（名称+类型 / 连接串全宽）
- **全站统一下拉**：自研 TypeSelect（圆角 10px 面板 + 当前项高亮），原生 select 零残留
- **12 套主题**：深色默认/薄荷青/暖金琥珀/冰蓝极光/紫夜星辰/绯红之夜/黑曜石/深海蓝/翡翠森林/樱花/落日橙/鎏金（palette 全套变量切换）
- **仪表盘面板**：系统日志 = 等宽日志流（时间+级别色标+操作者前缀，级别筛选/30s 轮询/自动滚动/上翻暂停）；最近动态 = 时间线（彩色圆点+相对时间+点击跳转+查看全部）
- 键盘快捷键：Alt+D/U/N/S/K、?、Esc

## 三、安全规范

- 存储型 XSS：闸门页全量 html.EscapeString + icon data URL 白名单；注册用户名字符集白名单（字母/数字/下划线/点/连字符/中文）
- SQL 注入：表名/列名白名单校验（Redis key 独立字符集）；pkCol 主键列名同 gate（2026-08-15 全量验收实锤阻断全表 UPDATE 注入）
- nginx 配置注入：base_path 字符集白名单，name 换行清洗（2026-08-15 补）
- 日志端点：项目存在性 + 查看权校验，阻断路径穿越与跨项目日志读取（2026-08-15 补）
- 状态机：项目状态白名单 online/offline/maintenance（2026-08-15 补）
- 聚合日志/统计：非超管仅见自身审计与可访问项目（2026-08-15 补）
- 项目成员接口：CheckProjectView 越权检查（2026-08-15 补）
- 记住我：仅存用户名，不持久化明文密码（2026-08-15 补）
- 备份路径穿越：safeBackupPath 目录边界 + 项目前缀校验
- 进程环境：子进程过滤 DATABASE_URL/JWT_SECRET
- 配置：secrets 不可经 API 读写

## 四、兼容性

| 目标 | 验证状态 |
|------|----------|
| MySQL 5.5（phpStudy） | ✅ 全 CRUD + 中文 |
| MongoDB 8.0 | ✅ 全 CRUD + 嵌套文档 |
| Redis 5.0/8.0 | ✅ 五类型 |
| PostgreSQL 16 | ✅ 全 CRUD |
| Qdrant 1.9 | ✅ 检索语义验证 |
| Go 1.25 | ✅ |

## 五、测试规范

- 单元测试：`go test ./...`（19 用例：auth JWT/限流、数据源路由、键列校验、nginx 生成）
- 前端 E2E：Playwright 42 用例（mock API 组件测试），CI 双 job（build + e2e）
- 真机验证：全量 API 矩阵 33/0（6 项目真库）+ 浏览器全页面，零 mock
- 提交规范：conventional commits

## 六、全量验收记录

**2026-08-15 全量深度验收**（白盒全读 + 黑盒矩阵 + 浏览器全流程 + 独立客户端压测，零 mock）：
- 架构评审：分层无环依赖、工厂加源成本低；主要债务 procmgr.go SRP、前端重复标记（遗留）
- 白盒黑盒互证实锤 3 CRITICAL：pkCol SQL 注入（全表 UPDATE 复现）、跨项目日志读取（公开路径复现）、nginx 配置注入（代码实锤）→ 已修复并复验
- 修复 25 项（后端 16 + 前端 9），修复后全量回归：矩阵 33/0、扩展攻击面 11 组全过、e2e 42/42、自愈 30s 验证、独立客户端 26-51rps p50 370ms 健康
- 已知边界（诚实清单）：XFF 轮换可绕 Go 层限流（nginx 层 5r/m + 防火墙兜底）、viewer 可见系统健康指标、改密后旧 token 有效至过期（8h）、登录 403 揭示停用账号、存量乱码用户名 1 条、Web1 静态目录新旧构建并存

## 六、已知边界（诚实清单）

- 忘记密码需 SMTP 配置后真实发信（未配置时 503 明确提示）
- DocView/KVView/VectorView 依赖真库实例（Windows mongod、App1 redis/qdrant 按需壳）
- 移动端布局已验证 375px；更小屏与弱网断网场景未全自动化
- git remote：私有仓库 lambs-server / lambs-web（SSH key 待用户授权后推送）
