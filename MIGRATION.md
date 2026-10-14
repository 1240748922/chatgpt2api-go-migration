# 本地 Go 迁移说明

本地迁移实现及模拟环境回归已完成。版本为 `3.3.3-go.local`，尚未发布镜像，未部署服务器；真实上游与 Docker 容器端到端验收仍需执行。

- 原项目：`chatgpt2api-31000`，基线 `ae3886665e91aceea0cf366b4c28d7ff55093e4f`（3.2.51）。
- Go 基线：`gptGrok2api`，提交 `f91732d`。
- 工作分支：`codex/go-feature-migration`。
- 本地目录：`D:\SDNT\gptGrok2api-local-migration`。
- 原 Python 工作目录保留原状。开发和测试没有连接服务器，也没有复制真实账号、凭据、生产数据库或生产配置；仓库仅推送代码与示例配置。

## 功能迁移

| 范围 | 实现及行为 |
| --- | --- |
| 生图与编辑 | Go 后端提供 OpenAI 格式的图片生成、编辑与图片聊天，保留原 Go 项目的 Grok 能力；支持 URL/b64 返回、多个图片模型与任务归属隔离。 |
| 共享账号池 | PostgreSQL 保存稳定账号 ID、AT/RT 去重索引、修订与删除标记；增量快照更新候选池，不依赖所有请求重写全量账号 JSON。 |
| 多实例并发 | 默认单 `app` 实例；需要时用 Compose `--scale app=N` 扩容。生图槽位、账号使用、凭据续期采用跨实例租约；以数据库时间、租约 fencing 与条件写入避免双重占用、旧结果覆盖。 |
| 凭据准入 | 未知、过期、有效期不足的 AT 不用于新生图；界面、派发与维护共用有效期预算。有效 RT 需先续期并验证新 AT。 |
| RT 轮换 | 刷新前持久化不确定状态；成功后先保存轮换 RT，再验证。响应丢失时保护旧 RT，不反复重放；RT 失效不直接否定仍有效的 AT。 |
| 导入 | AT/RT/JSON/JSONL 与 Session JSON 自动提取；Session JSON 可粘贴或读取本地 JSON 文件，确认后提交，支持 accessToken、auth_session、tokens、response.session，并保留可用于恢复的 ID token、登录密码、2FA 和 session cookie。分批入库、持久任务；关闭窗口或刷新后继续查询。文件自带代理、额度、冷却不直接导入。注册机邮箱 refresh_token 不会被误当作 OpenAI RT。 |
| 进度与日志 | 入库、额度同步分别显示完成数和总数；按邮箱展示逐账号结果，日志分页及区域滚动，窗口支持关闭、最小化、恢复。 |
| 维护与可用性 | 性能驱动批量、并发、间隔；公平恢复顺序；显示待维护、同步中、本批进度、凭据状态与文/图生图就绪额度。 |
| 页面预热 | 有界只读初始化缓存，以 AT+出口区分；不提前提交生图。输入图上传与初始化可重叠。 |
| 错误处理 | 上传限流冷却、鉴权待核验、额度耗尽持久化；区分中断、超时、文本结果、交付失败；安全的提交前失败才重新发起生成。 |
| 任务恢复 | 保存提交检查点；重启恢复只查询已有任务，避免因下载/存储失败再次生成；可编辑任务有独立查询预算。 |
| 图片与备份 | 原子文件、幂等交付、缩略图、保留周期；WebDAV 队列、R2/加密备份；远端未确认前保留本地。PG 一致快照恢复到空库，拒绝覆盖已有数据。 |
| 注册机 | 保留原 Python 工具，Go 管理持久任务，由独立 registrar 执行；日志、导入、额度同步；注册代理不绑定成功账号。 |
| 图片处理 | 默认 Go Lanczos3；可选独立服务支持 Sharp、Pillow、FSRCNN。 |
| 管理与配置 | Vue 页面接入迁移后端，共享鉴权、稳定 OAuth 加密密钥；品牌“Chatgpt生图系统”，项目链接指向用户仓库。 |
| 网关 | Docker DNS 动态解析，保留 `/api/` 路径；管理和图片 API 都发往 `app` 服务，由 Docker DNS 感知 `--scale app=N` 的副本，不在网关重放生成请求。 |
| 更新后导航 | 模块缺失时显示重新加载入口，避免旧页面点击菜单无反馈；已提交后台任务继续执行。 |

核心后端已迁为 Go；注册工具及可选图片处理仍保留 Python/Node 独立进程。生图 API 不转发给原 Python 服务。界面使用 Go 项目 Vue 样式，不保证旧配置文件逐字段原样兼容。

## 运行规则

- 凭据就绪、就绪额度不等于空闲并发。上传冷却、正在生图、模型匹配等仍影响派发。额度基于最近验证，未知额度单列；文生图、图生图额度有重叠，不能相加。
- 维护正常策略：每批 24 个、4 并发、间隔 200ms。性能压力增大时逐级降低为 8/2/1s、4/1/2s、1/1/5s；严重压力或用户关闭时可暂缓。
- 不确定续期、可恢复和处理中账号不会简单作为死号删除。清理前重新检查账号版本、租约和恢复条件；自动清理默认关闭。
- 默认单账号图片并发 1、全局 128。增加并发不能增加上游额度或解除限流。
- 每个 app/registrar 进程连接池默认 8 条；默认单 app + registrar 合计最多 16 条。使用 `--scale app=N` 时按 `(N+1)×GO_DATABASE_MAX_CONNECTIONS` 核算，并给 PostgreSQL 保留管理和维护余量。
- 预热只执行准备阶段，但会访问上游；缓存不能保证每次命中。

## 已完成的验证

日期：2026-10-14。账号与上游均为本地构造，测试数据库仅在本机。

| 验证 | 结果与证据 |
| --- | --- |
| Go 全部测试 | `go test ./... -count=1` 通过，含本地 PG 集成、模拟 Python 注册；`.local-checks/go-test-complete.log`。 |
| 静态检查 | `go vet ./...` 通过；`.local-checks/go-vet-complete.log`。 |
| Linux 构建 | CGO 关闭、Linux amd64 编译通过；另用 Dockerfile 同系列 Go 1.25.0 编译通过；`.local-checks/docker-toolchain-build.log`。 |
| 前端 | TypeScript/Vite/服务端静态文件构建通过；`.local-checks/vue-build-complete.log`。 |
| 规模 | 20,000 个模拟账号、8 个逻辑副本、320 次派发与并行写入，未发现同槽位重复占用；导入 3.94s，取号 P50 166.83ms、P95 754.78ms、最大 962.26ms；`.local-checks/scale-test.log`。本机模拟数据不是生产吞吐承诺。 |
| 凭据与恢复 | AT 准入一致性、RT 不确定保护、条件写入、删除与使用冲突、额度去重、重启任务不重复提交、租约失效等回归通过。 |
| 注册 | 模拟子进程部分成功后失败，成功账号仍入库并同步；未执行真实注册。 |
| 处理引擎 | 真实 Sharp/Pillow/FSRCNN 输出 64×40、透明通道保留、无需放大直通、非法尺寸拒绝、Sharp 不可用回退；`.local-checks/processor-engines-check.log`。 |
| 浏览器导入 | 自动提取 AT/RT，32+64 个账号入库与同步完成；64 个任务启动后关闭窗口、切页、刷新，回来仍完成 64/64。96 个模拟账号均无导入代理。 |
| 浏览器管理 | 账号/日志/配置/注册/可用性展示验证，预热 16/16；故意移走模块后提示失败，恢复文件、刷新后菜单正常。 |

截图：`.local-checks/import-progress.png`、`account-availability.png`、`navigation-module-failure.png`。测试依赖、数据、日志、截图与二进制均忽略，不进入提交或镜像。

尚未执行：Docker/Compose 实际构建部署、真实上游生成/编辑/注册、真实 WebDAV/R2、Go race 检测。本机缺少 Docker 环境及 race 所需 C 工具链。模拟回归不能证明真实网络、上游协议和生产服务器表现。

## 独立部署与验收

不复用 Python 部署的账号、数据卷或 `.env`。默认端口 31001，项目名 `chatgpt-images-go`，可与原服务并行。

1. 在独立目录准备本分支完整源码，执行 `cp .env.go.example .env.go`。
2. 为 `POSTGRES_PASSWORD`、`CHATGPT2API_AUTH_KEY`、`CHATGPT2API_ADMIN_KEY`、`GO_OAUTH_ENCRYPTION_KEY` 分别填写新值，可用 `openssl rand -hex 32` 生成。hex 数据库密码无需 URL 转义。加密密钥后续保持不变。
3. URL 图片返回需设置实际 `GO_PUBLIC_BASE_URL`；端口可改 `CHATGPT_GO_PORT`。默认单实例构建启动：

   ```bash
   docker compose --env-file .env.go -f docker-compose.go.yml build
   docker compose --env-file .env.go -f docker-compose.go.yml up -d
   docker compose --env-file .env.go -f docker-compose.go.yml ps
   curl -i http://127.0.0.1:31001/health
   curl -i http://127.0.0.1:31001/version
   ```

   需要扩容时改用 Compose 的服务扩容，不要复制 `app0` 到 `app7`：

   ```bash
   docker compose --env-file .env.go -f docker-compose.go.yml up -d --scale app=4
   ```

4. 用新管理员密钥登录，自行导入账号、重新设置图片处理/存储/注册供应商/邮箱/代理等。注册镜像包含浏览器，首次构建下载较大。
5. 少量账号先验收文生图、带输入图编辑、AT/RT 导入续期、真实注册、URL 访问、日志、备份，再逐步扩大并发和池大小。
6. 重建或扩缩 `app` 后，未登录 `/api/dashboard` 应返回 401 而非 404，登录后可加载。网关配置使用动态解析，设计上应用 IP 变化无需重载；实际扩缩容行为仍须在 Docker 环境验收。修改宿主机网关配置文件时重建网关重新绑定：

   ```bash
   docker compose --env-file .env.go -f docker-compose.go.yml up -d --force-recreate gateway
   ```

   网关重建会短暂中断；应用更新要等健康检查，不承诺零中断。

本迁移版本没有 GHCR 镜像。原项目 `git pull`/`docker compose pull` 不会自动部署它；后续发布需明确仓库、分支和镜像标签。

## 更新与回退

源码更新后重新构建，再 `up -d`；保留 `.env.go` 和加密密钥。上游任务依靠检查点恢复，不保证任意强制终止都无损。

回退时把业务入口切回保留的 Python 服务，再停止这套 Go 部署：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml down
```

不要带 `-v`，保留 Go 数据卷。两套库互相独立，新导入账号和状态不会自动回写 Python 库。

## 开发者复验

使用 Go 1.25、Node 24、可选 Python；准备可创建临时 schema 的本地 PG 测试库。测试创建并清理独立 schema，拒绝远程数据库。Windows 按本机路径设置程序，命令含义相同。

```bash
export MIGRATION_TEST_DATABASE_URL='postgres://migration_test@127.0.0.1:55439/postgres?sslmode=disable'
export MIGRATION_TEST_PYTHON='/path/to/python3'
go test ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/chatgpt-images-go ./cmd/gptgrok2api
(cd web-vue && npm ci && npm run build:server)
MIGRATION_SCALE_TEST=1 go test ./internal/accounts -run TestPostgresTwentyThousandAccountsEightReplicas -count=1 -v
```

未设置 PG/Python 测试环境变量，对应集成测试会跳过；跳过不能作为集成验收通过。
