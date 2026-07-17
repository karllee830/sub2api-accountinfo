# sub2api-accountinfo

嵌入 Sub2API 自定义菜单的订阅账号用量页面。Sub2API 打开 iframe 时传入 `user_id` 和用户 JWT Token，本项目会：

1. 使用 Token 调用 Sub2API `/auth/me` 实时验证用户身份；
2. 校验 Token 中的实际用户 ID 与传入的 `user_id` 一致；
3. 使用仅保存在服务端的 Admin API Key 查询用户的有效订阅；
4. 查询每个订阅分组绑定的全部账号；
5. 展示账号平台、账号类型和 5h、7d 等用量窗口；
6. 对 OpenAI 账号提供剩余重置次数查询，并通过 `ALLOW_RESET` 和用户自定义属性 `allow_reset` 控制是否显示重置按钮。

浏览器不会收到 Admin API Key。`src_host` 和 `src_url` 只作为 Sub2API 提供的来源信息存在，本项目不会使用它们选择上游地址，避免将管理凭证发送到非预期站点。

## 配置

复制示例配置：

```bash
cp .env.example .env
```

| 环境变量 | 必填 | 说明 |
| --- | --- | --- |
| `SUB2API_URL` | 是 | Sub2API 根地址，程序会自动补 `/api/v1`；也可直接填写 API 地址 |
| `SUB2API_ADMIN_API_KEY` | 是 | Sub2API 后台生成的 Admin API Key，用于服务端 `x-api-key` 鉴权 |
| `ALLOW_RESET` | 否 | 默认 `false`，仅允许用户属性 `allow_reset=true` 的用户重置；设为 `true` 时忽略用户属性并允许全部已验证用户重置 |
| `TRUST_PROXY_HEADERS` | 否 | 是否信任反代传入的客户端 IP，默认 `true`；用于保持 Sub2API Token 的 IP/UA 会话绑定 |
| `FRAME_ANCESTORS` | 否 | CSP `frame-ancestors` 来源列表；默认使用 `SUB2API_URL` 的 origin |
| `LISTEN_ADDR` | 否 | 容器内监听地址，默认 `:8080` |
| `HOST_PORT` | 否 | Docker Compose 映射到宿主机的端口，默认 `8080` |

`SUB2API_ADMIN_API_KEY` 可在 Sub2API 管理后台的“设置 → 安全 → Admin API Key”中生成。完整 Key 只在生成时显示一次。

旧版的 `ACCESS_TOKEN` 和 `ACCOUNT_IDS` 已删除，不再需要配置。账号访问范围完全根据已验证用户的有效订阅分组实时计算。

重置权限支持两种模式：

- `ALLOW_RESET=true`：全局允许，忽略 Sub2API 用户自定义属性，所有通过身份验证且能访问目标账号的用户都会显示并允许使用重置按钮；
- `ALLOW_RESET=false`（默认）：全局默认禁止，仅当已启用 key 为 `allow_reset` 的用户自定义属性，且当前用户值为 `true` 时允许重置。值为 `false`、未设置、格式无效或属性未启用时均不显示按钮，服务端也会拒绝请求。

## Sub2API 嵌入

在 Sub2API 自定义菜单中把 URL 配置为本项目地址。Sub2API 会生成类似链接：

```text
https://accountinfo.example.com/?user_id=2&token=USER_JWT&theme=light&lang=zh&ui_mode=embedded&src_host=...
```

前端读取 Token 后会立即从地址栏移除 `token`，并仅保存在当前标签页的 `sessionStorage` 中。后续请求通过 `Authorization: Bearer ...` 发送。

Token 默认带有 Sub2API 会话 IP/UA 绑定，因此反向代理必须传递原始客户端信息：

```nginx
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header User-Agent $http_user_agent;
```

当 `TRUST_PROXY_HEADERS=true` 时，不要把容器端口直接暴露到不可信网络；应只允许可信反代访问。

## Docker 运行

直接使用 Docker Hub 镜像：

```bash
docker pull karllee830/sub2api-accountinfo:latest
docker compose up -d
```

原有 GHCR 镜像仍会同步发布：

```bash
docker pull ghcr.io/karllee830/sub2api-accountinfo:latest
CONTAINER_IMAGE=ghcr.io/karllee830/sub2api-accountinfo:latest docker compose up -d
```

或从当前源码构建：

```bash
docker compose up -d --build
```

健康检查地址为 `GET /healthz`，不需要用户 Token。

## 对接的 Sub2API 接口

用户身份验证：

- `GET /api/v1/auth/me`

服务端通过 Admin API Key 调用：

- `GET /api/v1/admin/users/:id/subscriptions`
- `GET /api/v1/admin/user-attributes?enabled=true`（仅 `ALLOW_RESET=false`）
- `GET /api/v1/admin/users/:id/attributes`（仅 `ALLOW_RESET=false`）
- `GET /api/v1/admin/accounts?group=:group_id`
- `GET /api/v1/admin/accounts/:id/usage`
- `GET /api/v1/admin/accounts/:id/usage?source=active&force=true`
- `GET /api/v1/admin/openai/accounts/:id/quota`
- `POST /api/v1/admin/openai/accounts/:id/reset-quota`（需通过全局开关或用户属性授权）

次数和重置接口仅用于 OpenAI 账号。每次次数或重置请求都会重新验证用户 Token，并重新确认目标账号仍属于该用户的有效订阅分组；当 `ALLOW_RESET=false` 时，重置请求还会实时读取 `allow_reset`，防止通过修改账号 ID 或前端响应越权访问。

## 本地验证

```bash
go test ./...
go build ./...
node --check web/app.js
```

## 自动发布镜像

工作流 `.github/workflows/publish-ghcr.yml` 会并行构建 `linux/amd64` 和 `linux/arm64` 镜像。AMD64 使用标准 runner，ARM64 使用原生 `ubuntu-24.04-arm` runner；每个平台只构建一次并同时推送到 GHCR 和 Docker Hub，完成后分别合并为多架构 manifest：

- 推送到 `main`：发布 `latest`、`main` 和 `sha-*` 标签；
- 推送 `v1.2.3` 格式的 Git 标签：发布 `1.2.3`、`1.2`、`1` 和 `sha-*` 标签；
- 也可在 GitHub Actions 页面手动运行。

镜像地址分别为 `ghcr.io/karllee830/sub2api-accountinfo` 和 `docker.io/karllee830/sub2api-accountinfo`。GHCR 使用仓库自动提供的 `GITHUB_TOKEN`；Docker Hub 需要仓库 Secret `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`。
