# sub2api-accountinfo

一个只展示指定 Sub2API OpenAI OAuth 账号用量窗口的小型只读入口。页面行为参考 Sub2API 的 `/admin/accounts`：

- 自动显示 5h、7d 等用量窗口；
- “查询”按钮强制主动刷新用量；
- “次数”按钮查询可用的 rate-limit reset credits；
- “重置”按钮是否可操作由 Docker 环境变量控制；
- Sub2API 管理凭证只在服务端使用，不会下发到浏览器。

## 配置

复制示例配置：

```bash
cp .env.example .env
```

| 环境变量 | 必填 | 说明 |
| --- | --- | --- |
| `ACCESS_TOKEN` | 是 | 当前项目的访问口令，放在访问 URL 中；建议使用长随机字符串 |
| `ACCOUNT_IDS` | 是 | 可访问的账号 ID 白名单，多个 ID 用英文逗号分隔 |
| `SUB2API_URL` | 是 | Sub2API 根地址，程序会自动补 `/api/v1`；也可直接填写 API 地址 |
| `SUB2API_ADMIN_API_KEY` | 是 | Sub2API 后台生成的 Admin API Key，用于 `x-api-key` 鉴权 |
| `ALLOW_RESET` | 否 | `true` 允许重置，默认 `false`；服务端也会强制校验，不能只靠前端绕过 |
| `LISTEN_ADDR` | 否 | 容器内监听地址，默认 `:8080` |
| `HOST_PORT` | 否 | Docker Compose 映射到宿主机的端口，默认 `8080` |

`SUB2API_ADMIN_API_KEY` 可在 Sub2API 管理后台的“设置 → 安全 → Admin API Key”中生成。完整 Key 只在生成时显示一次；本项目仅通过服务端请求头 `x-api-key` 使用它，不会发送给浏览器。

## Docker 运行

直接使用 GHCR 镜像：

```bash
docker pull ghcr.io/karllee830/sub2api-accountinfo:latest
docker compose up -d
```

或从当前源码构建：

```bash
docker compose up -d --build
```

访问格式：

```text
http://服务器地址:8080/{ACCESS_TOKEN}/{ACCOUNT_ID}
```

例如：

```text
http://127.0.0.1:8080/replace-with-a-long-random-token/1
```

只有 `ACCOUNT_IDS` 中的 ID 可以访问。错误的口令和未授权账号统一返回 `404`，不会暴露白名单信息。

## 对接的 Sub2API 接口

服务端仅代理以下固定接口，不是通用代理：

- `GET /api/v1/admin/accounts/:id/usage`
- `GET /api/v1/admin/accounts/:id/usage?source=active&force=true`
- `GET /api/v1/admin/openai/accounts/:id/quota`
- `POST /api/v1/admin/openai/accounts/:id/reset-quota`（仅 `ALLOW_RESET=true`）

当前“次数/重置”能力对应 Sub2API 的 OpenAI OAuth 账号。其他平台即使能返回部分用量数据，也不保证支持这两个操作。

## 本地验证

```bash
go test ./...
go build ./...
```

健康检查地址为 `GET /healthz`，不需要访问口令。

## 自动发布镜像

工作流 `.github/workflows/publish-ghcr.yml` 会并行构建 `linux/amd64` 和 `linux/arm64` 镜像。AMD64 使用标准 runner，ARM64 使用原生 `ubuntu-24.04-arm` runner；两个 Job 分别推送 digest，完成后再合并为同一个多架构 manifest：

- 推送到 `main`：发布 `latest`、`main` 和 `sha-*` 标签；
- 推送 `v1.2.3` 格式的 Git 标签：发布 `1.2.3`、`1.2`、`1` 和 `sha-*` 标签；
- 也可在 GitHub Actions 页面手动运行。

镜像地址固定为 `ghcr.io/karllee830/sub2api-accountinfo`，发布使用仓库自动提供的 `GITHUB_TOKEN`，无需额外配置 GHCR 密码。
