# 包级 var 审计表

来源：QA 第 8 轮校准候选 5（全局状态审计只修了 db.DB，其余未审计）。
审计方式：`grep -n "^var" --include="*.go"` 全仓逐项分类。结论：4 处测试污染点已加恢复惯例，其余按类处置。

## 可变状态（测试可污染）

| var | 包 | 突变点 | 风险 | 处置 |
|---|---|---|---|---|
| `JWTKey` | auth | main 启动 + 3 个测试覆盖 | 测试忘恢复 → 跨测试污染（db.DB 同类） | 测试加 `t.Cleanup` 恢复（本轮） |
| `config` | notify | main `SetConfig` + 测试 | 同左 | smtp_roundtrip 测试加恢复（本轮）；notify_test 已自管 |
| `DB` | db | `db.Init` + 真库测试 | 已修（#62）：测试尾 `db.DB = nil` 恢复 | 合规 |
| `clockTicks` | runtime | `init()` 一次性（getconf） | 仅启动突变，测试不触 | 安全 |
| `PortMgr`/`ProcMgr`/`TCPProxyMgr` | runtime | init 构造，运行期内部突变 | 测试只读 | 观察 |
| `lambsConfig`/`cpuState` | main | 主循环持有 | root 包测试不经 main 入口 | 安全 |
| `adminChats`/`bot`/`wakeCh` | cmd/tg-bot | 测试经 botOps 注入 | 已有注入点 | 合规 |

## 只读配置（不突变）

`db.httpClient`、`tgbackup.httpClient`、`source_rest.restHTTPClient`、`web1Host`/`gateHost`/`sshRun`（sshRun 已是注入点）、`backupBaseDir`、`projectLogDir`、`trustedProxies`、`loginLimiter`、`allowedSources`、`readAllMax`、`retryDelay`。

- HTTP client ×3：当前无测试需要替换 → 暂不注入（YAGNI，需要时沿用 sshRun 函数注入模式）。
- env 启动快照类（hosts/dir/proxies）：进程生命周期只读 → 安全。

## 不可变（regex/常量/字面量）

`safeTableName`、`usernameRe`、`journalRe`、`safeColName`、`keywordHostRe`、`blockedEnvPrefixes`、`sensitiveKeySubstrings` — 永不突变 → 安全。

## 惯例

1. 测试修改包级状态 → 先存旧值，`t.Cleanup` 恢复（本轮统一）。
2. 新测试想替换行为 → 优先已有注入点（botOps / sshRun / SetConfig），不新增包级 var。
3. 新增包级可变 var 前先查本表 — 若可做成 env 启动快照或函数注入，不做包级 var。
