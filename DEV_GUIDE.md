# sub2api 项目开发指南

> 本文档记录项目环境配置、常见坑点和注意事项，供 Claude Code 和团队成员参考。

## 一、项目基本信息

| 项目 | 说明 |
|------|------|
| **上游仓库** | MACOS-DO/sub4api |
| **Fork 仓库** | bayma888/sub2api-bmai |
| **技术栈** | Go 后端 (Ent ORM + Gin) + Vue3 前端 (pnpm) |
| **数据库** | PostgreSQL 16 + Redis |
| **包管理** | 后端: go modules, 前端: **pnpm**（不是 npm） |

## 二、本地环境配置

### PostgreSQL 16 (Windows 服务)

| 配置项 | 值 |
|--------|-----|
| 端口 | 5432 |
| psql 路径 | `C:\Program Files\PostgreSQL\16\bin\psql.exe` |
| pg_hba.conf | `C:\Program Files\PostgreSQL\16\data\pg_hba.conf` |
| 数据库凭据 | user=`sub2api`, password=`sub2api`, dbname=`sub2api` |
| 超级用户 | user=`postgres`, password=`postgres` |

### Redis

| 配置项 | 值 |
|--------|-----|
| 端口 | 6379 |
| 密码 | 无 |

### 开发工具

```bash
# golangci-lint（CI 用 v2.13，本地建议装同一版以免版本差异带来的噪音）
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13

# pnpm (前端包管理)
npm install -g pnpm
```

## 三、CI/CD 流水线

### GitHub Actions Workflows

| Workflow | 触发条件 | 检查内容 |
|----------|----------|----------|
| **backend-ci.yml** | push, pull_request | 单元测试 + 集成测试 + golangci-lint v2.13 |
| **security-scan.yml** | push, pull_request, 每周一 | govulncheck + gosec + pnpm audit |
| **release.yml** | tag `v*` | 构建发布（PR 不触发） |

### CI 要求

- Go 版本必须是 **1.27.0**：三个 workflow 都用 `go-version-file: backend/go.mod` 取版本，随后硬断言 `go version | grep -q 'go1.27.0'`。升级 Go 时要同时改 `backend/go.mod`、`backend-ci.yml`（两处）、`release.yml`、`security-scan.yml` 里的这句断言，**以及三个 Dockerfile 里的 Go 构建镜像**（`Dockerfile` / `deploy/Dockerfile` 的 `ARG GOLANG_IMAGE`、`backend/Dockerfile` 的 `FROM golang:`）。前者漏了 CI 会在版本校验步骤直接失败；**后者漏了 CI 不会报，而是等到有人用这些 Dockerfile 构建时才失败**（`go.mod requires go >= X (running Y; GOTOOLCHAIN=local)`）。
- 前端使用 `pnpm install --frozen-lockfile`，必须提交 `pnpm-lock.yaml`

### 本地测试命令

```bash
# 后端单元测试
cd backend && go test -tags=unit ./...

# 后端集成测试
cd backend && go test -tags=integration ./...

# 代码质量检查
cd backend && golangci-lint run ./...

# 前端依赖安装（必须用 pnpm）
cd frontend && pnpm install
```

### OpenAI / Codex 协议兼容性

公共 OpenAI API（API key）与 ChatGPT Codex 后端（OAuth/PAT）不是同一契约。
行为对照基线为 `openai/codex` 提交 `1fb5158b3496a05abb89fb992d45737a02511d47`；
ccodex README 仅用于发现差异，不能替代官方源码。主要依据是
`codex-rs/core/src/client.rs`、`core/src/session/mod.rs`、`protocol/src/responses_metadata.rs`
和 `login/src/auth/manager.rs`。

- OAuth/PAT Responses Lite：工具和非空 instructions 转为输入前缀，保留已有历史工具声明、工具结果、schema 与显式 tool_choice；图片 detail 仅在图片内容项上移除，不递归改写工具 schema。API key 不套用此私有转换。
- 普通 OpenAI/Codex HTTP、透传与 WS 请求在缺失时补齐 Codex 风格的 `client_metadata`、`x-codex-turn-metadata`、session/thread/window headers；已有调用方字段优先，整数与 compaction 元数据保持精度。未提升的非 OpenAI 兼容供应商 legacy compact body 不新增普通 `client_metadata`。
- `environment_context` 的 `timezone` 与 `current_date` 使用账号固定 IANA 时区生成；已有客户端时区会被替换，缺失字段会补齐，日期按该时区重新生成。默认时区为 `Asia/Singapore`，配置不覆盖客户端之外的非 OpenAI 平台。
- Codex identity fallback 使用本机 Windows 基线 `Windows 10.0.22621.1848; x86_64; dumb`，并与当前 Codex client version、originator、version header 同源；管理员显式 UA 设置仍可覆盖指纹部分。
- `/responses/compact` 对 OpenAI 账号在 API ingress 转成当前 native Responses compaction v2：`stream=true`、`store=false`、末尾 `compaction_trigger`、`remote_compaction_v2`；非 OpenAI 兼容供应商保留原 legacy compact 路径。
- 普通 HTTP 与透传统一 session/thread/turn 元数据，身份隔离包含调用方和上游凭据；工具续接保留真实 turn_id/时间，不凭每次 HTTP 请求生成新 turn。OpenAI 账号的 legacy compact 输入先转为 native `/responses` compaction v2，因此走 regular Responses zstd；非 OpenAI 兼容供应商 legacy compact、API key 其他域名与 WS 帧不套用 zstd。重试的 GetBody 和 ContentLength 必须与实际发送内容一致。
- OAuth 提前五分钟刷新，优先读取 JWT 到期时间；推理 HTTP 与三个 WS 握手入口遇 401 最多原账号恢复一次。永久拒绝按凭据版本记忆；临时错误不能当作永久失效，API key/PAT/AgentIdentity 不进入 refresh-token 恢复。模型清单拉取与人工账号测试有独立发送链路，不能把推理恢复覆盖范围外推到这些探针。
- 不应因第三方 README 删除合法 beta/turn-state：官方会声明已启用的 RemoteCompactionV2，且同 turn 的重试与续接可以携带 turn-state；跨账号隔离应针对具体 opaque token，而不是覆盖整段会话的最后一个 token。

定向回归（在 `backend` 目录）：

```bash
go test -tags=unit ./internal/service ./internal/repository ./internal/handler ./internal/pkg/openai -run 'OpenAI|Codex|OAuthRefresh|HTTPUpstream' -count=1
```

协议兼容不等于与官方二进制逐字节相同，也不能证明账号不会被限制。Go 的 TLS/HTTP 栈、
部署出口及运营方式仍可能不同；不自动伪造工作区、sandbox 或遥测。离线回归与本地抓包
不代表真实 OpenAI 账号验收，不能用本文推断未公开的风控规则。

GPT-6.1 Sol (`gpt-6.1-sol`) is included in the OpenAI catalog, model whitelist, Codex normalization, Responses/Chat reasoning guards, and billing fallbacks. The billing fallback follows the official API contract: $2/M uncached input, $0.10/M cached input, $2.50/M cache creation, $10/M output, 2x fast pricing, and the 272K long-context multiplier. Upstream model metadata remains authoritative when available.

GPT-6.1 Sol is reasoning-only: explicit `none`, `minimal` or disabled thinking is rejected rather than silently upgraded. Supported API efforts are `low`, `medium`, `high`, `xhigh`, and `max`. Its pinned [official Codex descriptor](https://github.com/openai/codex/blob/b1e72963c3b71a9265a551e54beff078384efed9/codex-rs/models-manager/models.json) defaults to `low` and advertises Responses Lite/code mode for official ChatGPT OAuth; API-key catalogs keep plain Responses. The Codex `model is not supported when using Codex with a ChatGPT account` message does not prove a plan restriction: check the effective version, not just account type or the bundled descriptor's minimum version. With the same account and Lite/low request, `0.158.0` returned this 400 and `0.159.3` completed successfully. Manual version overrides take precedence over the synchronized value; when automatic synchronization is disabled, an old synchronized value remains in effect until an explicit version is set. Reference: [cc-switch #7797](https://github.com/farion1231/cc-switch/issues/7797).

### Responses 服务端 Web Search

这是中转站托管的工具，不是客户端函数工具。开启后，客户端不必注册搜索工具，
普通 `POST /v1/responses` 请求即可让模型选择搜索；模型选择不搜索时正常返回回答。
协议类型与输出格式参考 [OpenAI Web search](https://developers.openai.com/api/docs/guides/tools-web-search)。

启用顺序：

1. 系统设置 → 网关 → Web Search 模拟：配置 Brave/Tavily 并打开全局开关。
2. 渠道管理 → 编辑 → 对应平台标签（例如 OpenAI）→ 关联分组下面的“Web Search 模拟”，或将 API Key 上游账号设为“开启”。OAuth 账号通过渠道控制，不显示账号级覆盖开关。
   账号“默认”跟随渠道，“关闭”覆盖渠道；账号“开启”仍不能绕过全局关闭。
   渠道搜索项始终可见；全局未开启/未配置服务商时禁用并提示，加载失败显示“重新加载”，不会隐藏整行或丢失已保存的渠道值。每次打开编辑弹窗重新读取全局状态。
3. 客户端发送普通 Responses 请求，例如：

   ```json
   {"model":"your-model","input":"查一下今天的发布信息并附来源","stream":true}
   ```

执行与兼容边界：

- 全局关闭、账号关闭、渠道未开启时不接管，不移除或改写客户端原来的搜索工具。
  原生上游搜索仍由原转发链路处理。显式 `tool_choice: "none"` 不启用托管搜索。
- 启用时网关向上游模型提供内部搜索函数；由模型生成查询，Brave/Tavily 返回真实结果，
  再回填模型继续生成。客户端函数工具可以共存，仍由客户端执行。
- 对外返回标准 `web_search_call`、来源 URL 和回答；不暴露内部搜索函数或要求客户端执行它。
  回答中实际引用来源 URL 时添加 `url_citation`，不为没有引用的文字伪造引用。
- 客户端显式声明 `web_search` / `web_search_preview` 也由网关接管。
  支持 `search_context_size`、`filters.allowed_domains` 和 `external_web_access: true`；
  不支持的搜索约束返回明确错误，不静默忽略。域名限制同时用于查询和结果过滤。
- 尊重强制客户端函数与 `allowed_tools` 限制；内部搜索满足 required 后，仅放宽继续生成的
  必选要求，不扩大 allowed_tools 范围。默认最多 8 次搜索，可用 `max_tool_calls` 设置；
  `max_output_tokens` 跨模型轮次扣减。用量与计费结果累计真实模型轮次的 token。
- JSON 与 SSE 均支持。接管请求先收集每个模型轮次，再组装单个 Responses 结果和标准 SSE
  生命周期；这是缓冲交付，不是逐 token 直通。关闭接管时原流式转发不变。
  Codex 流中若终止事件的 `output` 为空，使用已经收到的 `response.output_item.done` 按索引恢复完整输出；非空的终止输出仍优先，避免丢失搜索调用或回答。
- 保留最终上游 response ID。公共搜索项重放和“同轮服务端搜索 + 客户端调用”的续接结果
  按用户/API Key/分组隔离，保存在当前进程的有界搜索缓存中（1 小时，最多 4096 条/64 MiB）。
  进程重启或淘汰后，重放过期的网关搜索项会报错，应改为发送消息形式的上下文。
- 覆盖 HTTP Responses 的原生 OpenAI 兼容链路、Chat/Anthropic 上游桥接及 Antigravity 桥接；
  不接管 WebSocket、`/responses/compact`、`/responses/input_tokens`、Chat/Messages 原生入口。
  托管搜索不支持 `background: true`，启用接管时明确拒绝；关闭功能后仍按原链路转发。
  原 Anthropic“纯搜索请求”快捷模拟保持不变。

定向回归：

```bash
go test -tags=unit ./internal/service ./internal/handler ./internal/pkg/apicompat ./internal/pkg/websearch -run 'HostedResponses|ResponsesSearch|GetWebSearchEmulationMode|WebSearch|ForwardResponses|ForwardAsResponses|ResponsesStream|ResponsesClientTools' -count=1
```

### Claude Code / Anthropic 转发兼容性

Claude Code 2.1.283 的转发基线应优先保证协议语义，而不是逐字节仿冒：

- Anthropic SSE 必须按空行聚合；多条 `data:` 用换行拼接；流内错误使用 `event: error`；不完整 `message_stop` 不能提前视为终止。
- OAuth 请求只改写明确需要隔离的身份字段；`metadata.user_id` 的 `parent_session_id`、`tk` 和未来扩展字段必须保留。真实存在的 Claude Code agent/request 头可以透传，但不得无条件合成。
- `messages` 与 `count_tokens` 必须保持客户端识别、beta、system 和 provider 路由一致。Vertex service-account 的计数请求走 Vertex Anthropic `count-tokens:rawPredict`。
- `ProxyID`、`custom_base_url`、Vertex/Bedrock/provider 配置是实际出口选择，必须保留账户级自定义地域和代理；不得用伪造 IP、地区头、遥测或机器标识规避上游策略。
- 不自动转发 Claude Code 产品遥测，不把代理自己的 user/device ID 注入上游；模型请求上下文、产品遥测和可选 OTEL 详细记录是三条不同数据路径。
- 真实 Claude Code 的 billing attribution 保留客户端版本和后缀，不再按账号缓存 UA 重算。该保留规则不关闭其他 metadata/fingerprint 设置。
- OAuth mimic 指纹按 JavaScript UTF-16 下标采样；在 system 迁移前保存原始 user 文本，重试/出站同步仍使用该文本，避免把代理插入的指令当作用户消息。mimic 保留 `cch=00000`。
- mimic 默认运行环境取自本机 Node 实测：`Windows / x64 / node / v22.19.0`，SDK 版本为提取包中的 `0.112.1`。这是固定兼容配置，不代表部署服务器实际运行 Node；已有账号指纹缓存不自动清空。

本地回归：

```bash
go test -tags=unit ./internal/service ./internal/handler ./internal/repository ./internal/pkg/claude ./internal/pkg/anthropicfp -run 'Test(.*(Claude|Anthropic|Gateway|Metadata|Identity|Streaming|Passthrough|CountTokens|OAuth|Cache|Beta|Thinking|Tool|SSE|HTTPUpstream|CLIVersion|Billing).*)' -count=1
```

## 四、常见坑点 & 解决方案

### 坑 1：pnpm-lock.yaml 必须同步提交

**问题**：`package.json` 新增依赖后，CI 的 `pnpm install --frozen-lockfile` 失败。

**原因**：上游 CI 使用 pnpm，lock 文件不同步会报错。

**解决**：
```bash
cd frontend
pnpm install  # 更新 pnpm-lock.yaml
git add pnpm-lock.yaml
git commit -m "chore: update pnpm-lock.yaml"
```

---

### 坑 2：npm 和 pnpm 的 node_modules 冲突

**问题**：之前用 npm 装过 `node_modules`，pnpm install 报 `EPERM` 错误。

**解决**：
```bash
cd frontend
rm -rf node_modules  # 或 PowerShell: Remove-Item -Recurse -Force node_modules
pnpm install
```

---

### 坑 3：PowerShell 中 bcrypt hash 的 `$` 被转义

**问题**：bcrypt hash 格式如 `$2a$10$xxx...`，PowerShell 把 `$2a` 当变量解析，导致数据丢失。

**解决**：将 SQL 写入文件，用 `psql -f` 执行：
```bash
# 错误示范（PowerShell 会吃掉 $）
psql -c "INSERT INTO users ... VALUES ('$2a$10$...')"

# 正确做法
echo "INSERT INTO users ... VALUES ('\$2a\$10\$...')" > temp.sql
psql -U sub2api -h 127.0.0.1 -d sub2api -f temp.sql
```

---

### 坑 4：psql 不支持中文路径

**问题**：`psql -f "D:\中文路径\file.sql"` 报错找不到文件。

**解决**：复制到纯英文路径再执行：
```bash
cp "D:\中文路径\file.sql" "C:\temp.sql"
psql -f "C:\temp.sql"
```

---

### 坑 5：PostgreSQL 密码重置流程

**场景**：忘记 PostgreSQL 密码。

**步骤**：
1. 修改 `C:\Program Files\PostgreSQL\16\data\pg_hba.conf`
   ```
   # 将 scram-sha-256 改为 trust
   host    all    all    127.0.0.1/32    trust
   ```
2. 重启 PostgreSQL 服务
   ```powershell
   Restart-Service postgresql-x64-16
   ```
3. 无密码登录并重置
   ```bash
   psql -U postgres -h 127.0.0.1
   ALTER USER sub2api WITH PASSWORD 'sub2api';
   ALTER USER postgres WITH PASSWORD 'postgres';
   ```
4. 改回 `scram-sha-256` 并重启

---

### 坑 6：Go interface 新增方法后 test stub 必须补全

**问题**：给 interface 新增方法后，编译报错 `does not implement interface (missing method XXX)`。

**原因**：所有测试文件中实现该 interface 的 stub/mock 都必须补上新方法。

**解决**：
```bash
# 搜索所有实现该 interface 的 struct
cd backend
grep -r "type.*Stub.*struct" internal/
grep -r "type.*Mock.*struct" internal/

# 逐一补全新方法
```

---

### 坑 7：Windows 上 psql 连 localhost 的 IPv6 问题

**问题**：psql 连 `localhost` 先尝试 IPv6 (::1)，可能报错后再回退 IPv4。

**建议**：直接用 `127.0.0.1` 代替 `localhost`。

---

### 坑 8：Windows 没有 make 命令

**问题**：CI 里用 `make test-unit`，本地 Windows 没有 make。

**解决**：直接用 Makefile 里的原始命令：
```bash
# 代替 make test-unit
go test -tags=unit ./...

# 代替 make test-integration
go test -tags=integration ./...
```

---

### 坑 9：Ent Schema 修改后必须重新生成

**问题**：修改 `ent/schema/*.go` 后，代码不生效。

**解决**：
```bash
cd backend
go generate ./ent  # 重新生成 ent 代码（json.RawMessage 字段会生成为同类型的 jsontext.Value，属预期）
git add ent/       # 生成的文件也要提交
```

---

### 坑 10：前端测试看似正常，但后端调用失败（模型映射被批量误改）

**典型现象**：
- 前端按钮点测看起来正常；
- 实际通过 API/客户端调用时返回 `Service temporarily unavailable` 或提示无可用账号；
- 常见于 OpenAI 账号（例如 Codex 模型）在批量修改后突然不可用。

**根因**：
- OpenAI 账号编辑页默认不显式展示映射规则，容易让人误以为“没映射也没关系”；
- 但在**批量修改同时选中不同平台账号**（OpenAI + Antigravity/Gemini）时，模型白名单/映射可能被跨平台策略覆盖；
- 结果是 OpenAI 账号的关键模型映射丢失或被改坏，后端选不到可用账号。

**修复方案（按优先级）**：
1. **快速修复（推荐）**：在批量修改中补回正确的透传映射（例如 `gpt-5.3-codex -> gpt-5.3-codex-spark`）。
2. **彻底重建**：删除并重新添加全部相关账号（最稳但成本高）。

**关键经验**：
- 如果某模型已被软件内置默认映射覆盖，通常不需要额外再加透传；
- 但当上游模型更新快于本仓库默认映射时，**手动批量添加透传映射**是最简单、最低风险的临时兜底方案；
- 批量操作前尽量按平台分组，不要混选不同平台账号。

---

### 坑 11：PR 提交前检查清单

提交 PR 前务必本地验证：

- [ ] `go test -tags=unit ./...` 通过
- [ ] `go test -tags=integration ./...` 通过
- [ ] `golangci-lint run ./...` 无新增问题
- [ ] `pnpm-lock.yaml` 已同步（如果改了 package.json）
- [ ] 所有 test stub 补全新接口方法（如果改了 interface）
- [ ] Ent 生成的代码已提交（如果改了 schema）

## 五、常用命令速查

### 数据库操作

```bash
# 连接数据库
psql -U sub2api -h 127.0.0.1 -d sub2api

# 查看所有用户
psql -U postgres -h 127.0.0.1 -c "\du"

# 查看所有数据库
psql -U postgres -h 127.0.0.1 -c "\l"

# 执行 SQL 文件
psql -U sub2api -h 127.0.0.1 -d sub2api -f migration.sql
```

### Git 操作

```bash
# 同步上游
git fetch upstream
git checkout main
git merge upstream/main
git push origin main

# 创建功能分支
git checkout -b feature/xxx

# Rebase 到最新 main
git fetch upstream
git rebase upstream/main
```

### 前端操作

```bash
# 安装依赖（必须用 pnpm）
cd frontend
pnpm install

# 开发服务器
pnpm dev

# 构建
pnpm build
```

### 后端操作

```bash
# 运行服务器
cd backend
go run ./cmd/server/

# 生成 Ent 代码
go generate ./ent

# 运行测试
go test -tags=unit ./...
go test -tags=integration ./...

# Lint 检查
golangci-lint run ./...
```

## 六、项目结构速览

```
sub2api-bmai/
├── backend/
│   ├── cmd/server/          # 主程序入口
│   ├── ent/                 # Ent ORM 生成代码
│   │   └── schema/          # 数据库 Schema 定义
│   ├── internal/
│   │   ├── handler/         # HTTP 处理器
│   │   ├── service/         # 业务逻辑
│   │   ├── repository/      # 数据访问层
│   │   └── server/          # 服务器配置
│   ├── migrations/          # 数据库迁移脚本
│   └── config.yaml          # 配置文件
├── frontend/
│   ├── src/
│   │   ├── api/             # API 调用
│   │   ├── components/      # Vue 组件
│   │   ├── views/           # 页面视图
│   │   ├── types/           # TypeScript 类型
│   │   └── i18n/            # 国际化
│   ├── package.json         # 依赖配置
│   └── pnpm-lock.yaml       # pnpm 锁文件（必须提交）
└── .claude/
    └── CLAUDE.md            # 本文档
```

## 用户请求审计记录

用户请求审计的分组记录开关默认全部关闭（空的 `group_ids` allowlist）。管理员必须在“用户请求审计”页面选择分组并显式保存；页面可选择启用或停用的分组。只会记录保存选择之后发生的请求，已有审计记录不会因修改 allowlist 被删除。

审计配置接口为 `GET/PUT /api/v1/admin/user-request-audit/config`。`group_ids` 是分组 ID 数组：显式发送 `[]` 会清空 allowlist 并停止所有新记录；PUT 省略该字段时保留现有选择。保留天数、清理间隔和分片大小等归档设置与该 allowlist 分开维护。

## 七、参考资源

- [上游仓库](https://github.com/MACOS-DO/sub4api)
- [Ent 文档](https://entgo.io/docs/getting-started)
- [Vue3 文档](https://vuejs.org/)
- [pnpm 文档](https://pnpm.io/)
