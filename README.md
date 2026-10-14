# Chatgpt 生图系统 · Go 迁移版

当前版本：`3.3.3-go.local`。本仓库是独立的本地迁移实现，**不是已验收的生产版本**；尚未发布 Docker 镜像。

以 Go 实现生图/编辑接口、账号池、AT/RT/Session JSON 导入、续期、额度同步、后台维护、预热、任务与媒体存储；注册工具及可选图片处理保留独立 Python/Node 服务。原 Go 项目的 Grok 功能保留。前端为 Vue 管理界面。

完整功能范围、已执行测试、未完成的真实环境验收、独立部署和回退步骤见 [MIGRATION.md](MIGRATION.md)。

## 新服务器部署

以下命令面向 **Linux 服务器、Bash、root 用户**，按步骤逐段执行。新服务器自行导入账号，不复制旧服务器的账号库或配置。

> 使用本仓库的 `.env.go` 和 `docker-compose.go.yml`。当前没有发布本项目的预构建镜像，需要在服务器上从源码构建，单独执行 `docker compose pull` 不会完成部署。默认端口为 `31001`，Compose 项目名为 `chatgpt-images-go`，使用独立的 PostgreSQL 数据卷。

### 1. 检查环境

```bash
git --version
openssl version
curl --version
docker info
docker compose version
docker buildx version
```

需要 Git、OpenSSL、curl、已启动的 Docker Engine，以及支持 `up --wait` 的 Docker Compose 插件和 Buildx。使用 `docker compose`，不是旧版 `docker-compose`。如有命令缺失，先用 `cat /etc/os-release` 确认发行版，再按 [Docker Engine 官方安装说明](https://docs.docker.com/engine/install/)安装对应组件；环境检查通过后再继续。

### 2. 下载项目

首次部署使用一个新的目录：

```bash
mkdir -p /opt &&
cd /opt &&
git clone --branch main https://github.com/1240748922/chatgpt2api-go-migration.git &&
cd /opt/chatgpt2api-go-migration
```

后续命令均在 `/opt/chatgpt2api-go-migration` 执行。目录已经存在时，不要重复克隆，请先确认它是本仓库，再按“后续更新”操作。

### 3. 创建配置与密钥

以下命令生成四个不同的随机密钥；已有 `.env.go` 时跳过，避免覆盖现有配置：

```bash
(
  set -eu
  umask 077

  if [ -e .env.go ]; then
    echo '.env.go 已存在，保留现有配置。'
    exit 0
  fi

  tmp_env="$(mktemp .env.go.setup.XXXXXX)"
  trap 'rm -f -- "$tmp_env"' EXIT
  cp .env.go.example "$tmp_env"

  for key in \
    POSTGRES_PASSWORD \
    CHATGPT2API_AUTH_KEY \
    CHATGPT2API_ADMIN_KEY \
    GO_OAUTH_ENCRYPTION_KEY
  do
    value="$(openssl rand -hex 32)"
    sed -i "s/^${key}=.*/${key}=${value}/" "$tmp_env"
  done

  chmod 600 "$tmp_env"
  mv -n -- "$tmp_env" .env.go
  echo '配置及随机密钥已生成，请设置外部访问地址。'
)
```

编辑配置：

```bash
vi /opt/chatgpt2api-go-migration/.env.go
```

修改以下两项，将示例中的公网 IP 换成真实地址：

```dotenv
CHATGPT_GO_PORT=31001
GO_PUBLIC_BASE_URL=http://你的服务器公网IP:31001
```

已有配置好的 HTTPS 域名时，`GO_PUBLIC_BASE_URL` 填写 `https://你的域名`，不要追加 `/v1`。这个地址需要让调用方能访问，用于返回图片链接；仅填写域名不会自动配置 DNS、证书或反向代理。

从 `3.3.3-go.local` 起，也可在「系统设置 → 基础配置 → 图片访问地址」保存同样的公网前缀，供新请求使用。优先级为 `GO_PUBLIC_BASE_URL` → `CHATGPT2API_BASE_URL` → 后台 `base_url`；部署变量已填写时，以部署变量为准。后台设置无需重启，修改 `.env.go` 后需执行 Compose `up -d` 使新环境变量进入容器。已创建的图片任务继续使用创建时保存的地址。

若日志或接口返回 `/v1/files/image?id=...`，这是站内相对链接，不是服务器磁盘路径；说明当时未取得有效的完整访问前缀。已有图片可在该路径前补上本服务公网地址访问，无需重新生成。未显式配置公网地址时，程序默认不信任任意公网 Host/转发头，以免返回被伪造的地址；不要为此关闭该保护。

| 配置 | 用途 |
| --- | --- |
| `CHATGPT2API_ADMIN_KEY` | 管理后台登录密钥 |
| `CHATGPT2API_AUTH_KEY` | API 调用密钥 |
| `POSTGRES_PASSWORD` | 这套部署的数据库密码 |
| `GO_OAUTH_ENCRYPTION_KEY` | OAuth 数据加密密钥，后续保持不变 |

请妥善备份 `.env.go`，不要发到聊天、截图或提交 Git。更新时不要重新生成密钥；已有数据库也不会因为修改环境变量而自动更换密码。

### 4. 构建并启动

```bash
docker compose --env-file .env.go -f docker-compose.go.yml config --quiet &&
docker compose --env-file .env.go -f docker-compose.go.yml build &&
docker compose --env-file .env.go -f docker-compose.go.yml up -d --wait --wait-timeout 180 &&
docker compose --env-file .env.go -f docker-compose.go.yml ps -a
```

首次构建需要下载依赖和注册机浏览器，耗时取决于网络与机器性能。任一步失败时先查看错误，不要跳过构建直接启动。

默认是 **一个 `app` 应用实例**，不是整个系统只有一个容器；还会运行 PostgreSQL、网关、注册任务进程和图片处理服务。`volume-init` 是一次性初始化容器，执行成功后显示 `Exited (0)` 属正常现象。

### 5. 检查服务并登录

```bash
curl --max-time 10 -i http://127.0.0.1:31001/health
curl --max-time 10 -i http://127.0.0.1:31001/version
```

如果修改了端口，上述命令也要同步调整。健康检查应返回 HTTP 200；版本信息应与当前检出的 `VERSION` 一致。

在浏览器打开 `http://你的服务器公网IP:31001`，或已配置好的 HTTPS 域名。仅在自己的服务器终端查看管理员登录密钥：

```bash
grep '^CHATGPT2API_ADMIN_KEY=' /opt/chatgpt2api-go-migration/.env.go
```

这条命令会显示完整密钥，不要分享输出。登录后自行导入 AT / RT / Session JSON，并按需配置注册机、存储等功能。

首次验收建议在云安全组中仅允许自己的 IP 访问 TCP `31001`，对外使用前配置 HTTPS。Docker 发布端口可能绕过部分主机防火墙规则，请结合云安全组和 [Docker 防火墙说明](https://docs.docker.com/engine/network/packet-filtering-firewalls/)核对。

**先用少量账号验收生图、编辑、导入、续期等流程，再扩大使用。** Docker 实际部署、真实上游及扩缩容仍需独立环境验收，不将模拟测试当作生产可用性保证。

## 查看日志与排查启动失败

```bash
cd /opt/chatgpt2api-go-migration &&
docker compose --env-file .env.go -f docker-compose.go.yml ps -a

docker compose --env-file .env.go -f docker-compose.go.yml logs --tail=100 app gateway postgres registrar image-processor volume-init
```

持续查看应用日志：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml logs -f --tail=100 app
```

按 `Ctrl+C` 只退出日志查看，不停止容器。分享日志前移除 AT、RT、Cookie、密钥及密码。

## 后续更新

保留 `.env.go` 和数据卷，先备份重要数据，并选择可接受短暂中断的维护时段。不要先执行 `down`：

```bash
cd /opt/chatgpt2api-go-migration &&
git pull --ff-only &&
docker compose --env-file .env.go -f docker-compose.go.yml config --quiet &&
docker compose --env-file .env.go -f docker-compose.go.yml build &&
docker compose --env-file .env.go -f docker-compose.go.yml up -d --wait --wait-timeout 180 &&
docker compose --env-file .env.go -f docker-compose.go.yml ps -a
```

如果更新包含 `deploy/go-nginx.conf` 改动，再重建网关，重新绑定宿主机配置文件。该步骤会短暂中断访问：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml up -d --force-recreate --wait --wait-timeout 180 gateway
```

更新后再次执行健康检查、版本检查，并登录后台确认。原 Python 仓库的更新命令不会更新这套 Go 部署。

## 按需扩容

默认一个 `app`，不固定创建 `app0` 到 `app7`。确有需要时，按机器资源和数据库连接数扩容，例如四个应用副本：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml up -d --scale app=4 --wait --wait-timeout 180
```

账号池、导入任务和租约状态由 PostgreSQL 共享，不需要复制账号文件。每个 `app`/`registrar` 进程默认最多 8 条数据库连接；四个 `app` 加一个 `registrar` 合计最多 40 条，另需为管理和维护保留余量。副本数不等于上游额度，也不保证吞吐量线性提升。

扩容后在后续 `up` 命令中保留目标 `--scale app=N` 参数。回到单实例：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml up -d --scale app=1 --wait --wait-timeout 180
```

## 停止服务

```bash
cd /opt/chatgpt2api-go-migration &&
docker compose --env-file .env.go -f docker-compose.go.yml down
```

停止会中断服务。不要添加 `-v`，该选项会删除本项目的数据卷。两套项目的数据彼此独立，原 Python 部署不会自动获得新导入的 Go 账号及状态。

保留原项目许可文件 [LICENSE](LICENSE) 与 [GROK2API_LICENSE](GROK2API_LICENSE)。
