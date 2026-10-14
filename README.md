# Chatgpt 生图系统 · Go 迁移版

当前版本：`3.3.1-go.local`。本仓库是独立的本地迁移实现，**不是已验收的生产版本**；尚未发布 Docker 镜像。

以 Go 实现生图/编辑接口、账号池、AT/RT/Session JSON 导入、续期、额度同步、后台维护、预热、任务与媒体存储；注册工具及可选图片处理保留独立 Python/Node 服务。原 Go 项目的 Grok 功能保留。前端为 Vue 管理界面。

完整功能范围、已执行测试、未完成的真实环境验收、独立部署和回退步骤见 [MIGRATION.md](MIGRATION.md)。

## 独立试运行

本项目使用**独立**的 PostgreSQL 数据卷和默认端口 `31001`，不复用原 Python 项目的账号、配置或数据。用户自行导入账号。请勿把真实密钥提交到 Git。

```bash
cp .env.go.example .env.go
# 为 POSTGRES_PASSWORD、CHATGPT2API_AUTH_KEY、CHATGPT2API_ADMIN_KEY、
# GO_OAUTH_ENCRYPTION_KEY 填写四个不同的随机值。
docker compose --env-file .env.go -f docker-compose.go.yml build
docker compose --env-file .env.go -f docker-compose.go.yml up -d
docker compose --env-file .env.go -f docker-compose.go.yml ps
curl -i http://127.0.0.1:31001/health
```

默认只启动一个 `app` 实例，不再固定创建 8 个容器。需要扩容时按机器资源和数据库连接数按需设置，例如：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml up -d --scale app=4
```

账号池、导入任务和租约状态由 PostgreSQL 共享；扩容不需要复制或迁移账号文件。`GO_DATABASE_MAX_CONNECTIONS` 是每个进程的连接上限，扩容前请核算总连接数。

正式切换流量前，需要在独立环境验收 Docker 构建、真实生图/编辑、导入/续期、注册机和远程存储。`git pull` 或 `docker compose pull` 原 Python 项目不会部署此仓库。

保留原项目许可文件 [LICENSE](LICENSE) 与 [GROK2API_LICENSE](GROK2API_LICENSE)。
