# Cookie 交换接口实施与验收记录

日期：2026-10-03。状态：用户已授权发布检查、提交并合入部署分支。初次实施证据保留如下；本次复核与门禁修复见末节。业务机上线另按部署手册执行。

## 工作区与基线

- 功能分支：`feature/cookie-exchange`。
- worktree：`/root/projects/xianyu-helper-project/worktrees/feature-cookie-exchange`。
- 基线：`9c2f4a87e24740494867628f59099158b9d389f7`，包含实施计划。
- 已 fetch 核对 `origin/production`，本地 production 比远程多 5 个提交，使用本地较新基线；未更新 production 工作区。
- 权威接口依据：`docs/cookie-exchange-integration-plan.md`；Reader 对照：`/root/projects/xianyu-radar/docs/helper-cookie-consumption-plan.md`。

## 实际修改范围

新增生产实现与测试配对：

| 生产文件 | 同目录测试文件 | 职责 |
| --- | --- | --- |
| `internal/server/cookie_exchange_handlers.go` | `cookie_exchange_handlers_test.go` | 专用路由、具名 DTO、认证、请求限制、URL/时间/顺序检查、统一脱敏错误、no-store |
| `internal/application/account/cookie_exchange_service.go` | `cookie_exchange_service_test.go` | 消费者窄 Port、专用授权模型、提交后同步状态编排 |
| `internal/adapter/cookie_exchange_repository.go` | `cookie_exchange_repository_test.go` | 归属复核、凭证短锁、完整状态指纹、有序响应重放、既有加密保存和 Token 清理 |
| `internal/composition/cookie_exchange_services.go` | `cookie_exchange_services_test.go` | 构造与日志回调接线；独立放置以避免扩大已有 New 函数 |

已有文件仅作接线或登记：

- `internal/adapter/account_dependencies.go`：新增专用仓储构造入口。
- `internal/composition/application_services.go`：保存、构造、投影交换服务。
- `internal/composition/runtime/server_dependencies.go`：注入 transport Port。
- `internal/server/application_ports.go`：新增专用 Port、输入字段、构造与必需依赖校验。
- `internal/server/versioned_session_routes.go`：调用一次专用路由挂载。
- `internal/server/server_test.go`：补足测试构造的同一 Port 注入。
- `internal/server/openapi_response_contract_test.go`：登记真实成功场景。
- `api/openapi.yaml`、`frontend/shared/api-contract/generated/schema.ts`：两个 operation 及生成契约。

本记录为新增验收文档。无数据库 schema、迁移、前端页面、API Key、续期任务或平台业务代理变更。原登录、续期、滑块、engine、automation 和 MTOP 业务源文件无差异。

## 专用秘密导出边界

这两条接口是本任务明确授权的 Cookie 导出边界。`ExchangeCookie`、`ExchangeSnapshot` 和对应 transport DTO 仅用于该边界，不复用数据库模型，不向 AccountSummary、QR、普通账号详情、日志或错误响应加入明文凭证。完整 metadata、密码、IM accessToken 和 Helper 管理会话均不导出。

GET 和 POST 都复用 cookieAuth 和真实用户归属；管理员不能借此读取其他用户账号。no-store 在认证之前设置，失败响应也不缓存。Reader 模拟使用完全分离的 Helper 会话 Jar 和平台 Jar。

## 实现语义

- GET 只读取当前非空完整快照，不续期、不请求平台、不启用账号；缺失或明确空快照返回 `409 cookie_snapshot_unavailable`。
- 状态版本包含账号、扁平 Cookie、完整标记及归一化有序快照全部属性，排除刷新时间和无关 metadata；同秒属性变化仍冲突。
- POST 限制 256 KiB、1～32 批、每批 1～128 条、单头最多 8192 字节；未知字段、尾随 JSON、头部换行、非法来源和非法时间拒绝。
- 只接受文档固定的四个 HTTPS 主机及默认 443，无 URL 用户信息、query、fragment；UTC RFC3339 三位毫秒，过去十分钟至未来三十秒，批次时间不得倒序。
- 锁内复核归属和版本，逐跳按原 received_at 调用现有 ApplySetCookies，固定分区站点 `https://goofish.com`；保留同名作用域、HttpOnly、Secure、SameSite、分区和创建顺序。
- 完整 Jar 真正变化时才生成 Helper `/im` Cookie 字符串、保留其他 metadata 并复用 UpdateRenewalCookie；浏览器拒绝的无效头可以返回 changed=false。
- 仅 UPDATE 已存在账号；版本冲突不保存，首次改变后的重复 POST 返回 409。删除最后一条 Cookie 会保存明确完整空 Jar，后续导出返回 409。
- 保存后清理旧 Token；清理失败仅记录固定非敏感阶段。返回应用层之前释放凭证锁，再调用既有 RuntimeService/AccountRuntimePort 重读权威数据库；不强制重启连接，不启动缺失实例。
- 无变化为 not_needed，无实例为 not_running，同步成功为 synced；保存后同步失败或取消为 failed，提交仍成功，不要求重放旧增量。

## 验证结果

| 检查 | 结果 |
| --- | --- |
| `make api-generate`、`make api-check` | 通过；包含生成漂移、双向路由、全 operation 与真实成功响应、原兼容接口场景 |
| `make vet`、`make lint`、`make comments` | 通过；lint 0 issues；Go 与前端中文注释门禁通过 |
| `go test -race ./internal/composition ./internal/adapter ./internal/application/account ./internal/server -run TestCookieExchange -count=1` | 通过；另补执行等待锁后归属变化的读取和提交 race，两条路径均拒绝旧所有者 |
| `make test-server-race` | 通过；原生命周期与凭证并发 smoke 不变 |
| `npm --prefix frontend run typecheck` | 通过 |
| `make cover-frontend` 对应的 `npm --prefix frontend run test:coverage` | 92 个文件、550 项测试通过；statement 79.08% |
| `make cover-browser` | 单独复测通过；RUN_BROWSER_INTEGRATION=1，statement 64.1% |
| `go test ./...`、`make cover` | 应用/数据库/协议包通过；tools/architecturecheck 的既有源码门禁失败，详情见下文；普通 Go profile statement 81.4%，未设置 RUN_BROWSER_INTEGRATION，不能记为完整检查通过 |
| `make check` | 未通过：同一既有架构行数门禁；未绕过或降低门禁 |
| 临时服务器构建与启动 | 通过；空 SQLite、默认浏览器启用、/health 200，SIGTERM 退出码 0；未启用真实账号 |
| `git diff --check` | 通过 |

确定性覆盖包括 401、403、404、409 两类错误、400、413、500；缺失/空完整快照；版本属性与顺序、同秒变化、无关 metadata 不影响版本；并发同版本只保存一次；等待锁后的删除和归属变化；多跳覆盖、域/路径/同名 Cookie、HttpOnly、分区、精确过期删除、Max-Age 原时刻；保存失败保持原值、Token 清理失败不回滚、同步失败不重放、锁释放后复读权威值、离线不启动；存储加密、无权用户先拒绝再解密、普通账号与错误输出秘密隔离。

Reader 文档格式的真实本地 HTTP 模拟已完成 session/login → 获取快照 → 透传版本提交增量，不请求真实闲鱼。Reader 配套计划仍未实施客户端；本任务没有修改 Reader 项目，不能视为真实 Reader 上线联调。

专用 handler 和应用服务 statement 均为 100%；适配器为 95.2%；独立组合接线定向为 100%。适配器余下三个防御返回处理“首次归属查询后第二次读取发现删除/换 owner”以及“锁内 UPDATE 发现账号已删除”。规范同进程写入者共用锁时不可正常发生；没有为提高覆盖率引入绕过锁的非法生产写入或放宽检查。合法删除/转属等待锁场景与持久化错误均已有测试。跨进程写库的全局 CAS 不属于第一版保证。

### 原基线失败及环境处理

`internal/automation/events.go` 在基线中为 805 行，超过架构 800 行上限。新增功能已移除的独立 worktree 中，`make architecture` 与 `go test ./...` 的 `TestStageTwoRealSourceGate` 仍复现完全相同失败。因此 make check 和 make cover 的退出状态明确保留为失败，未修改原业务文件或门禁来掩盖。

首次浏览器集成在并发检查期间墙钟超出冻结轨迹断言；没有改动实现、时序或断言，单独完整重跑 make cover-browser 通过。首次前端 multipart 测试因沙箱禁止回环监听出现 EPERM，在允许本地监听的环境按原命令完整重跑通过。

Go 工具链在此 worktree 环境中尝试从非 Git 项目父目录读取 VCS 信息导致构建失败；本地验证二进制用 `go build -buildvcs=false` 构建，仅不嵌入自动 VCS 元数据，未改变代码或正式发布脚本。独立服务器首次缺少配套 browser-install，随后在 /tmp 构建项目已有安装器并复用现有 Chromium 缓存，保持浏览器启用后启动通过。

## 移除演练

在独立临时分支 `feature/cookie-exchange-removal-check` 和对应 worktree：

1. 复制功能 diff 与全部新增模块。
2. 反向撤销上述集中接线、测试构造和契约登记，删除新增生产与测试模块。
3. `git diff --exit-code 9c2f4a8` 通过，工作区与原基线完全一致；不需要回滚数据库。
4. 原服务器构建通过；全库回归只有上述已在原基线存在的架构检查失败，原应用/协议/数据库测试通过。

移除顺序明确包含接线撤销，不承诺直接删除仍被引用的文件能编译。演练不删除或替换任何既有业务能力，不使用降低门禁的白名单。

## 初次实施时的限制与提交安排（历史记录）

- 第一版仅保证单 Helper 写入实例的同进程账号锁；不支持共享数据库多个独立进程的全局 CAS。
- Cookie 快照结构与版本不证明平台登录有效；停用账号不会因交换请求自动获得后台续期。
- 无真实账号、真实闲鱼请求或生产调用；此次未配置 MySQL/PostgreSQL 测试连接，使用隔离 SQLite；现有加密保存实现未改。
- 无生产部署、标签、镜像发布、提交、推送或合并。
- 用户确认测试后再安排提交：优先以“应用服务+其测试”“适配器+其测试”“集中组合接线+其测试”“专用 handler+其测试”等职责组织小提交，并在每条消息中列出具体文件及行为。契约源和生成文件保持成对；与构造校验、路由激活相关的必要接线在依赖一致的提交中收口，避免为了恰好两个文件形成不可构建或契约不一致的中间版本。当前未执行任何提交。

## 2026-10-03 发布复核

用户明确授权检查功能工作区、完成发布检查、提交并合入部署版本，替代初次实施时等待确认的提交安排。本次部署版本指 `production` 分支；不切换业务机服务。

代码审查确认：新增能力仅在专用 Cookie 交换边界导出秘密，普通账号、QR、错误与日志保持原保护；归属复核及版本检查在同账号锁内，运行时同步在释放锁之后执行；没有新增数据库迁移、真实平台请求或账号启动。冻结浏览器、MTOP、engine、数据库实现无差异。

发布阻断修复：将 `extractOrderIDFromContent` 从 `internal/automation/events.go` 原样移到 `events_order_links.go`，补准确中文注释。解析逻辑、调用入口和已有测试不变，原文件从 805 行降至 783 行；架构 800 行门禁保持原值。此前的基线失败记录是历史事实，本次不再沿用为发布豁免。

本次执行环境显式复用 `/usr/local/go/bin`、`/tmp/cookie-exchange-bin` 及已有临时 Go 缓存，不安装或升级依赖。包含本地 HTTP 监听的检查在允许回环监听的环境执行，沙箱监听拒绝不计为通过。

- `make check`：通过，架构、OpenAPI、vet、lint（0 issues）、全库测试与 Go/前端中文注释门禁全部通过。
- `make cover-browser`：通过；`RUN_BROWSER_INTEGRATION=1`，浏览器 statement 64.1%，冻结实现与测试未修改。
- `make cover`：通过；未设置 `RUN_BROWSER_INTEGRATION=1`，Go statement 81.4%。
- Cookie 交换、服务器生命周期与凭证并发定向 `go test -race ... -count=1`：通过。
- 前端 `typecheck`、`test:coverage`、`build`：通过，92 文件、550 测试，statement 79.08%；重建后嵌入资源无 Git 差异。
- 真实 Reader 客户端、真实账号及闲鱼平台调用不在此次模拟验收内；MySQL 未配置，数据库 schema 未变。

提交按职责组织：门禁阻断修复单独提交；应用服务与测试；适配器与测试及构造入口；最终 transport、组合接线、OpenAPI 与生成契约、验收记录一起收口，避免半接线路由。最终合并使用 `--no-ff` 保留功能评审边界。
