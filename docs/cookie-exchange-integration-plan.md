# Helper 外部 Cookie 交换实施计划

日期：2026-10-03。状态：建议书，尚未实施接口或业务代码。

检查基线：生产分支 `cb559f8`。实施前以当时生产代码重新核对文件和能力，不直接在 `source/main` 或 `production` 开发。

对应 Reader 项目为 `/root/projects/xianyu-radar`，配套计划为 `docs/helper-cookie-consumption-plan.md`。本文是第一版交换接口的契约依据；Reader 计划不得自行改变字段或错误语义。

## 1. 目标和修改边界

只新增两项能力：外部项目获取当前 Cookie 快照；外部项目提交闲鱼响应中的 Cookie 增量更新。

Helper 继续负责账号登录、Cookie 的权威持久化、既有续期、Token 缓存和账号运行时。Reader 自己执行搜索、商品详情等闲鱼请求，持有本次操作的临时 Cookie 会话，收到更新后提交 Helper。临时会话是 HTTP 协议运行所需状态，不是 Reader 自建的 Cookie 管理系统。

第一版不新增数据库表、不新增续期任务、不代理闲鱼业务请求、不新增 API Key 管理、不增加管理页面，不改扫码、滑块、自动发货和原有业务接口的内部逻辑。Reader 的调用不能自动启动或启用账号，不能保证平台 Cookie 永久有效。

现有后台续期只处理满足既有启用条件的账号。部署时由操作者在 Helper 确认目标账号的登录、启用状态和续期配置；导出停用账号的快照不代表它会持续得到后台维护。

可移除性目标：新增功能文件删除后，只需撤销集中装配、路由和契约登记，就能恢复原有功能；不要求回滚数据库，不要求修改原有续期和业务模块内部算法。不能直接删除仍被引用的文件后期待编译通过，接线撤销属于明确的移除步骤。

## 2. 已有能力和复用限制

| 能力 | 已有位置 | 第一版使用方式 |
| --- | --- | --- |
| 读取 Cookie 和完整 metadata | `internal/db/cookie_scope.go` 的 `GetCookiePlatformRuntimeData` | 适配器读取、解密；不把整个 metadata 导出 |
| 账号凭证锁 | `internal/db/store.go` 的 `LockAccountCredentials` | 获取与回写都在同账号短锁内确定状态 |
| Cookie 作用域和删除处理 | `internal/xianyu/cookierefresh/cookies.go` 的 `ApplySetCookies` | 按每跳 URL 和原接收时间顺序重放 |
| 完整快照保存 | 同文件的 `MetadataWithSnapshot` | 保留 metadata 其他键 |
| 加密更新现有账号 | `internal/db/renewal.go` 的 `UpdateRenewalCookie` | 沿用现有编码器；未配置数据密钥时仍遵循原存储行为 |
| 清理连接 Token | `store.Tokens.Clear`、账号仓储相关 Port | 凭证实际改变后处理旧缓存 |
| 运行时同步 | `internal/adapter/account_runtime.go`、现有 `UpdateRunningCookie` 回调 | 提交完成、释放凭证锁后同步，不强制重启在线连接 |

不使用手动 Cookie 替换接口：它可能清除完整快照，不具备外部响应的 URL、接收时间和增量语义。

不直接构造 `renew.Result{SetCookies: ...}` 复用续期结果回写：续期结果的响应 URL、时间、批次是未导出字段，缺失时会按静默续期端点解释。第一版在新的交换适配器内调用已公开的 `ApplySetCookies`，不修改 `internal/xianyu/renew`，不把外部交换强行绑定到续期模块。

`last_refresh_at` 不是所有写入路径都保证唯一递增，不能当成严格并发版本。

## 3. 功能组织和集中接线

建议新增三个主要生产文件，具名 DTO 可放在 handler 文件，测试独立同目录保存：

| 新文件 | 负责内容 |
| --- | --- |
| `internal/server/cookie_exchange_handlers.go` | 两个 handler、专用路由挂载函数、请求响应 DTO、输入检查、统一 HTTP 错误 |
| `internal/application/account/cookie_exchange_service.go` | 获取和回写两个用例、消费者 Port、非敏感提交结果、提交后同步编排 |
| `internal/adapter/cookie_exchange_repository.go` | 归属复核、读取快照、版本计算、短锁、响应重放、加密保存和缓存清理 |

应用服务通过窄 Port 接收按账号执行的获取与提交能力。Cookie 秘密不放入通用 AccountSummary，也不扩大现有万能仓储接口。授权的导出快照使用专门的短生命周期类型或导出 Port，不能把 db/runtime 模型直接序列化。

已有文件的预期修改点限定如下，实际实现可合并接线但不能把业务散落进去：

1. `internal/adapter/account_dependencies.go`：增加交换适配器的构造入口。
2. `internal/composition/application_services.go`：构造服务、保存一个专用 Port、投影到 transport。
3. `internal/composition/runtime/server_dependencies.go`：把该 Port 注入 Server。
4. `internal/server/application_ports.go`：增加专用 Port 和构造期校验。
5. `internal/server/versioned_session_routes.go`：调用一次独立的路由挂载函数。
6. `api/openapi.yaml` 和生成的 `frontend/shared/api-contract/generated/schema.ts`：登记并生成两个 operation。

上述列表不是要求一定修改七份实现代码；优先沿用实际构造链，接线内容应只包含类型、构造、传入和注册。原登录、续期、engine、automation、MTOP 业务文件不应因这项功能增加分支。测试 fixture 的构造变更只做必要补充。

现有 `docs/architecture/dependency-rules.md` 禁止秘密进入普通 DTO。实施时需明确记录本功能是经用户授权的专用 Cookie 导出边界，仅允许这两个操作使用特定类型；不能放宽账号列表、QR、日志和其他响应的秘密保护。遵循既有主计划、中文注释、OpenAPI 门禁和冻结滑块规范；不重新开启已完成的重构阶段。

## 4. 认证方式

第一版复用 Helper 现有登录会话与账号归属校验，两个接口声明现有 `cookieAuth`。Reader 的后端使用 `POST /api/v1/session/login`，发送 `username`、`password`，接收并在内存保留 Helper 的认证 Cookie。

这份认证 Cookie 与闲鱼 Cookie 必须放在两个完全独立的 HTTP 客户端或容器中，绝不能转发到对方域名。401 后允许一次受控重登录，不持续尝试错误密码。403 不重新登录绕过权限。

建议使用能访问目标闲鱼账号的专门 Helper 用户；现有归属模型不支持凭空授予另一个用户的账号，实施不能自动创建管理员、转移账号 owner 或添加隐含越权。部署前确认该用户确实拥有目标账号。当前会话授权并非只读范围的独立项目 Token，本版明确接受这个范围，不声称实现了 API Key 权限体系。

认证资料由 Reader 部署配置提供，不经过浏览器输入或返回，不进入日志和源码。跨机器传输使用 HTTPS；同机受控回环部署可使用 HTTP。不为本功能新增登录管理页面或认证数据库迁移。

## 5. 固定接口契约

### 5.1 获取当前快照

`GET /api/v1/integrations/accounts/{account_id}/cookie-snapshot`

需要已认证会话和目标账号归属。只读取当前状态，不执行平台请求、不触发续期、不启用账号。

成功响应：

```json
{
  "account_id": "example-account",
  "credential_version": "v1:opaque-state-fingerprint",
  "snapshot_complete": true,
  "cookies": [
    {
      "name": "_m_h5_tk",
      "value": "EXAMPLE_TOKEN_TIMESTAMP",
      "domain": ".goofish.com",
      "path": "/",
      "expires": 1800000000,
      "httpOnly": false,
      "secure": true,
      "sameSite": "Lax",
      "partitionKey": ""
    }
  ]
}
```

Cookie 对象字段采用现有 BrowserCookie 的命名。`name`、`value`、`domain`、`path` 必需；其他字段为可选，默认值由协议说明定义：无正有效期表示会话 Cookie，布尔缺省 false，空分区键表示不分区。不返回密码、IM accessToken、完整 metadata、管理会话或调试原始响应。

只有存在非空完整快照时才返回成功；无快照或明确空快照返回 `409 cookie_snapshot_unavailable`，需要先在 Helper 中完成有效登录。结构存在不能证明账号未被平台撤销，reader 仍需按真实业务响应判断。

响应设 `Cache-Control: no-store`，不使用浏览器缓存或 CDN 缓存。接口只返回当前用户拥有的指定账号，不批量导出所有账号。

### 5.2 回写响应更新

`POST /api/v1/integrations/accounts/{account_id}/cookie-updates`

```json
{
  "credential_version": "v1:opaque-state-fingerprint",
  "responses": [
    {
      "response_url": "https://h5api.m.goofish.com/h5/example/1.0/",
      "received_at": "2026-10-03T08:00:00.000Z",
      "set_cookies": [
        "example=value; Domain=.goofish.com; Path=/; Max-Age=3600; Secure"
      ]
    }
  ]
}
```

`responses` 按响应接收顺序排列。每跳 URL 是当跳原请求地址，reader 去掉不影响路径作用域的 query 和 fragment，避免提交签名参数。`received_at` 使用 UTC RFC3339 毫秒；原 `Set-Cookie` 逐条保留，不合并为一个逗号分隔字段。不传业务正文、HTTP Authorization 或闲鱼请求中的整份旧 Cookie。

允许的响应主机第一版固定为 `h5api.m.goofish.com`、`www.goofish.com`、`passport.goofish.com`、`seller.goofish.com`，仅 HTTPS、默认 443、无 URL 用户信息。新增主机需两份文档和契约同步明确。顶层分区站点固定 `https://goofish.com`，不允许调用方任意改变分区上下文。URL 仅用于解析作用域，Helper 不访问该 URL。

建议边界：请求体不超过 256 KiB、1～32 个批次、每批 1～128 条、单条头不超过 8 KiB；接收时间不得晚于 Helper 时间 30 秒或早于 10 分钟。时间限制用于拒绝过期回放，并要求两端正常校时。空更新不提交。

结构损坏返回 400；合法结构中被浏览器规则拒绝的 Cookie 不进入 Jar，处理规则复用现有解析器。成功可返回 `changed=false`，不能将这种情况误认为登录态恢复成功。

成功响应：

```json
{
  "account_id": "example-account",
  "changed": true,
  "credential_version": "v1:new-opaque-state-fingerprint",
  "runtime_sync_status": "synced"
}
```

`runtime_sync_status` 枚举：`synced`、`not_running`、`not_needed`、`failed`。`failed` 表示凭证已保存，但运行时同步失败；仍返回提交成功，不要求 reader 重放旧增量，日志记录非敏感原因。不存在实例时不能为了同步启动实例。

统一错误沿用既有 envelope，具体字段以 OpenAPI 为准：401 未认证；403 无权账号；404 账号不存在；409 `credential_conflict` 或 `cookie_snapshot_unavailable`；400 格式/来源/时间不合法；413 超出体积；500 持久化等内部故障。错误不得包含 Cookie。

## 6. 版本和回写流程

`credential_version` 是 Helper 计算的状态指纹，建议 `v1:` 加 SHA-256。输入包含账号 ID、扁平 Cookie、快照完整标记和归一化快照的稳定序列化；保留 Cookie 顺序和全部作用域属性，因为它们会影响请求。排除无关 metadata 与刷新时间。Reader 只透传，不自己计算，不把此值当登录有效性证明。

流程：

1. 校验认证、请求边界和来源，随后在账号凭证锁内重新校验归属及账号存在性。
2. 读取最新凭证，要求完整快照，计算当前版本。
3. 版本不相等返回 409，完全不保存，也不把旧响应重贴到新版本。
4. 在当前快照上顺序执行 `ApplySetCookies(snapshot, response_url, headers, received_at, "https://goofish.com")`。
5. 根据更新快照生成 Helper 消息页 `/im` 的规范字符串，更新 metadata；仅实际状态改变才调用 `UpdateRenewalCookie`，不创建已删除账号。
6. 提交成功后尽力清理旧 Token，释放账号锁，再调用既有运行时同步。同步函数会重读权威数据库，不强制断开已认证连接。
7. 返回提交后的版本和同步状态。缓存清理/运行时同步失败不回滚已提交 Cookie，也不能伪报数据库写入失败。

第一版严格检查整个凭证版本，可能拒绝不同字段上的无害并发更新。这是为了不保存额外秘密快照的明确取舍，不直接复用需要初始快照的精细冲突判断。

重复 POST 不承诺同样返回 200；首次成功后版本已改变，重复请求会被 409 阻断。响应丢失或超时时，reader 应重新获取权威快照确认状态，不盲目重复提交，不换成新版本重新包装旧增量。真正的新版本指纹不等于单调计数器，不提供更新历史。

同进程现有账号锁可以复用。本计划基于一个 Helper 写入实例，不宣称解决多个独立进程共享数据库的全局 CAS；多实例写入属于后续单独设计。

## 7. 实施顺序和验收

1. 从最新 production 创建一个 feature worktree，读取 AGENTS、主计划、依赖规则、注释标准、冻结滑块规范；重新核对实际接线位置。
2. 一次明确两个 operation 的契约及秘密导出边界，新增功能文件，完成集中接线。不同步骤属于同一功能任务，不借机重构旧模块。
3. 使用本地数据库和模拟响应验证合并、权限、并发及失败语义。无需真实 Cookie 或真实闲鱼调用。
4. 验证 401、403、404、无快照、版本冲突、同秒凭证变化、域/路径/同名 Cookie、HttpOnly、分区、过期删除、Max-Age 原时刻、多跳顺序、已删除账号、重复 POST、持久化失败、同步失败和不启动停用实例。
5. 验证普通账号/QR/日志仍不泄露秘密；新增导出响应只在明确认证成功场景可出现秘密。
6. 执行 `make api-generate`、`make api-check`、架构和中文注释门禁、相关 Go 测试及凭证并发 race；提交前完成仓库要求的 `make check`。不修改冻结 CAPTCHA 文件、不降低门禁和覆盖率要求。
7. 与 Reader 的模拟契约测试联调；进行移除演练：撤销集中接线和契约，删除新模块后原有构造、路由及测试应恢复。演练在临时分支执行，不能用删除现有功能绕过测试。

实现完成应报告新增文件、已有文件修改点、实际验收和局限；提交前审查是否意外触及旧业务链。部署或生产更新不属于此计划自动授权范围。

## 8. Helper 实施提示词

> 请在 Xianyu Helper 项目实施 `docs/cookie-exchange-integration-plan.md`。先核对文档契约及当前 production 基线，按项目规范创建独立 feature worktree，不在 main 或 production 直接开发。只实现 Cookie 快照获取和响应增量回写两个接口。新增功能集中在专用 handler、应用服务和适配器中，现有文件只作集中接线及 OpenAPI 登记，复用会话认证、归属检查、凭证锁、Cookie Jar 合并、加密保存和运行时同步。不修改扫码、后台续期、滑块冻结行为、自动发货或原有闲鱼业务接口，不新增数据库表、API Key 管理或前端页面。严格按文档处理状态版本、响应 URL/时间、多跳、409、重复提交和保存后同步失败；Reader 不改变此契约。完成契约、权限、并发和回归验证，以及可移除性审查。不要部署生产。最终说明实际修改点、测试结果和保留的限制。
