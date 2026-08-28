# nginx_web

linux nginx web

## 环境要求

- 后端：Go（nginx 相关功能仅支持 Linux / Ubuntu 系统运行）
- 前端：Node.js + npm

## 本地开发

```bash
./start.sh                  # 一键启动前后端（每次启动会先杀掉上一次的实例）
./start.sh stop             # 停止所有服务
```

或分别手动启动：

```bash
cd server && go run .        # 后端，监听 :8080
cd web && npm run dev        # 前端，访问 http://localhost:5173（/api 已代理到 :8080）
```

注意：在 Windows / macOS 上开发时，`/api/nginx` 与 `/api/nginx/available`
会返回 `supported: false`，页面提示"该功能仅支持 Linux（含 Ubuntu）系统"，属预期行为。

## 部署到 Linux 服务器

```bash
# 在开发机交叉编译 Linux 可执行文件
cd server && GOOS=linux GOARCH=amd64 go build -o server-linux .

# 构建前端静态文件
cd web && npm run build      # 产物在 web/dist
```

将 `server-linux` 上传至服务器运行（监听 :8080），nginx 托管 `web/dist`
并将 `/api` 反向代理到 `127.0.0.1:8080`，示例：

```nginx
location /api/ {
    proxy_pass http://127.0.0.1:8080;
}
```

## 接口

- `GET /api/hello` — 连通性测试
- `GET /api/nginx` — 检测服务器已安装的 nginx（版本、执行目录）
- `GET /api/nginx/available` — 通过包管理器（apt / dnf / yum / apk）查询可安装的 nginx 版本
