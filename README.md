# nginx_web

linux nginx web

## 环境要求

- 后端：Go（nginx 相关功能仅支持 Linux / Ubuntu 系统运行）
- 前端：Node.js + npm

## 本地开发（WSL）

> 必须在 **WSL / Linux** 的 bash 终端中执行。脚本会检测环境，在 Windows 的
> Git Bash / PowerShell 中运行时会直接报错退出。

```bash
cd /mnt/d/DDD/xxx/code/nginx_web

./start.sh            # 启动（先干净停止旧实例，再拉起前后端）
./start.sh stop       # 停止所有服务
./start.sh restart    # 重启
./start.sh status     # 查看运行状态 + 端口 + 健康检查
```

启动后访问：

- 前端 <http://localhost:5173>
- 后端 <http://localhost:8080/api/hello>

### 可用环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | 后端端口，前端代理自动跟随。**显式指定后冲突不再自动避让** |
| `VITE_PORT` | `5173` | 前端端口 |
| `VITE_POLL` | `/mnt` 下自动 `1` | 强制开启文件轮询监听 |
| `SKIP_PORT_CLEAN` | `0` | 置 `1` 时停止阶段不清理端口残留进程 |
| `NO_COLOR` | 空 | 置 `1` 关闭彩色输出 |

示例：`PORT=8090 ./start.sh`

### 手动启动（不走脚本）

```bash
cd server && go run .                                  # 后端，监听 :8080
cd web && API_PORT=8080 npm run dev                    # 前端，访问 http://localhost:5173
```

注意：在 WSL 之外的系统（Windows / macOS）直接运行时，`/api/nginx` 与
`/api/nginx/available` 会返回 `supported: false`，页面提示"该功能仅支持
Linux（含 Ubuntu）系统"，属预期行为。

## WSL 排障

### 1. 端口被 Windows 侧程序占用（最常见）

WSL2 的 `localhostForwarding` 会把 Windows 已监听的端口也"占住"，
导致 WSL 内 bind 失败，日志里表现为：

```
listen tcp :8080: bind: address already in use
```

但 WSL 里 `ss -lptn | grep 8080` 却查不到任何进程——这个矛盾现象就是特征。

在 Windows 上确认占用者：

```powershell
netstat -ano | findstr :8080
```

三种处理方式，任选其一：

1. **换端口启动**（最省事）：`PORT=8090 ./start.sh`
   —— 未显式指定 `PORT` 时，脚本会自动向后顺延寻找可用端口。
2. 关闭占用 8080 的 Windows 程序后重试。
3. 在 Windows 的 `%USERPROFILE%\.wslconfig` 中写入以下内容，然后执行
   `wsl --shutdown` 重启 WSL：

   ```ini
   [wsl2]
   localhostForwarding=false
   ```

### 2. `bad interpreter` / `$'\r': command not found`

脚本被存成了 CRLF 换行。本项目已通过 `.gitattributes` 锁定 `*.sh` 为 LF。
若本地文件仍被转换，执行一次：

```bash
sed -i 's/\r$//' start.sh
```

如之前设置过 `core.autocrlf=true`，建议针对本仓库关闭：

```bash
git config core.autocrlf false
```

### 3. 改了代码但页面不刷新（HMR 失效）

项目位于 `/mnt/d/...` 时走的是 drvfs，不支持 inotify。脚本检测到项目在
`/mnt` 下会自动开启轮询监听；若仍无效，手动开启：

```bash
VITE_POLL=1 ./start.sh
```

追求性能可把项目复制到 WSL 原生文件系统（如 `~/nginx_web`）再开发。

### 4. 页面提示"未检测到 Nginx 安装"

WSL 里还没有 nginx，属正常结果。安装后可看到真实数据：

```bash
sudo apt update && sudo apt install -y nginx
```

### 5. 启动很慢或文件监听异常

说明用到了 Windows 的 `node.exe`（WSL 互操作），性能和监听都受影响。
脚本会打印警告提示。建议安装 WSL 原生 Node.js：

```bash
curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.39.7/install.sh | bash
# 重开终端后
nvm install --lts
```

### 日志位置

```bash
tail -f .run/backend.log
tail -f .run/frontend.log
```

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
- `POST /api/nginx/install` — 源码编译安装指定版本，立即返回 `taskId`（异步执行）
- `GET /api/nginx/install/status?taskId=xxx` — 查询安装/卸载进度、状态与日志（两种任务共用）
- `GET /api/nginx/installs` — 读取历史安装记录（配置路径、执行目录等）
- `POST /api/nginx/uninstall` — 卸载本工具编译安装的 nginx，立即返回 `taskId`（异步执行）
- `GET /api/nginx/sites` — 读取 `config.d` 下的站点列表（含 nginx 安装状态）
- `POST /api/nginx/sites/create` — 新增一个站点配置（域名 / 端口 / 根目录）
- `POST /api/nginx/sites/update` — 修改已有站点配置（按文件名定位，域名变更会换文件名）
- `POST /api/nginx/sites/delete` — 删除站点配置
- `POST /api/nginx/reload` — 校验并重载（必要时启动）nginx，使站点配置变更生效

## 安装功能

列表里「未安装」的版本旁有「立即安装」按钮，点击后按以下流程编译安装：

1. 准备源码目录 `/data/soft`
2. `apt-get install -y build-essential libpcre3-dev zlib1g-dev libssl-dev`
3. 下载 `https://nginx.org/download/nginx-<版本>.tar.gz`
4. 解压后 `./configure --prefix=/usr/local/nginx --with-http_ssl_module --with-http_v2_module --with-http_gzip_static_module`
5. `make -j$(nproc)` 与 `make install`
6. `nginx -t` 校验配置并启动服务
7. 创建软链 `/usr/bin/nginx`
8. 在 `nginx.conf` 的 http 块末尾加入 `include config.d/*.conf;` 并创建该目录
9. `nginx -s reload` 重载配置

其中第 8 步由程序直接改写配置文件完成（原流程中的 `vim` 无法在程序里交互执行）。

### 安装记录

安装完成后，配置与执行路径会写入 `/usr/local/nginx/nginx-web-install.json`，
页面上会出现「安装记录」区块直接展示，安装成功的弹窗里也会列出关键路径。

记录字段：安装时间、安装前缀、配置文件、站点配置目录（config.d）、可执行文件、
全局软链、日志目录、PID 文件、源码目录，以及当时的 configure 参数（便于复现编译）。

- 同一版本重复安装会**覆盖**该版本的旧记录，不同版本各自保留
- 记录按安装时间倒序，页面首条为最近一次安装
- 记录写入失败不会让安装判定为失败，只会在日志中留一条告警

### 注意事项

- **需要 root 权限**：后端进程非 root 时，安装步骤会因权限不足失败，日志会给出提示。
- **耗时较长**：编译通常 5~15 分钟，因此做成异步任务。前端每 2 秒轮询进度，
  关闭弹窗不会中断后端任务。
- **同一时刻只允许一个安装任务**，重复发起会返回「已有安装任务正在进行」。
- **端口冲突**：80 端口若已被占用，启动步骤会失败并在日志中说明原因。
- **版本格式校验**：后端只接受 `x.y.z`，其余字符会被剥离，防止命令注入。
- **config.d 写入是幂等的**：nginx.conf 中若已包含 `config.d` 则不会重复添加。
- **OpenSSL 3.0 兼容**：Ubuntu 22.04 起默认为 OpenSSL 3.0，它把 `HMAC_Init_ex`、
  `ENGINE_by_id`、`ENGINE_set_default` 等接口标记为废弃，而 nginx 1.24.0 及更早版本
  仍在调用；又因 nginx 编译时默认带 `-Werror`，这些告警会被升级为错误并中断编译
  （典型报错：`'HMAC_Init_ex' is deprecated: Since OpenSSL 3.0 [-Werror=deprecated-declarations]`）。
  因此 configure 统一追加 `--with-cc-opt=-Wno-error=deprecated-declarations`，
  只降级这一类告警，日志中仍然可见，不影响其他警告的判定。
- **重跑安装会先清理源码目录**：避免因上次失败残留的 `.o` 文件导致再次编译失败。

## 卸载功能

列表里「已安装」的版本旁有「卸载」按钮（红色、危险操作），点击后弹出二次确认，
确认后按以下流程卸载：

1. 通过已安装的 `/usr/local/nginx/sbin/nginx -s stop` 优雅停止
2. 若仍存活，`pkill -x nginx` 兜底结束全部 nginx 进程
3. 删除全局软链 `/usr/bin/nginx`
4. 删除整个安装目录 `/usr/local/nginx`（同时清掉其中的安装记录文件）
5. 删除源码目录 `/data/soft/nginx-<版本>`（清理失败仅告警，不影响卸载结果）

uninstall 与 install 共用同一套异步任务框架：`POST /api/nginx/uninstall` 返回
`taskId`，进度同样用 `GET /api/nginx/install/status?taskId=xxx` 轮询。卸载成功后
页面会自动重新检测，列表里的「已安装」标记与「安装记录」区块会立即更新。

### 卸载注意事项

- **不可恢复**：删除 `/usr/local/nginx` 目录即彻底清除编译产物，配置与记录一并消失，
  操作前请确认已备份所需配置。
- **只卸载本工具安装的部分**：本工具固定把 nginx 编译安装到 `/usr/local/nginx`，
  卸载对象也只限于此。若系统另存有包管理器（apt/dnf…）装到 `/usr/sbin/nginx` 的实例，
  本操作不会清理它（但第 2 步的 `pkill` 会把它一并停掉，必要时请改用包管理器卸载）。
- **需要 root 权限**：删除 `/usr/local/nginx` 与 `/usr/bin/nginx` 均需写权限，
  非 root 时可能失败，日志会给出提示。
- **同样受「同一时刻只允许一个任务」约束**：卸载与安装互斥，进行中再次发起会被拒绝。

## 站点配置功能

左侧菜单切到「站点配置」后，页面以列表展示 `config.d`（`/usr/local/nginx/conf/config.d`）
下的全部站点，每个 `.conf` 文件即一个站点，对应一个 `server { ... }` 块。列表字段为
配置文件名、域名 / IP（`server_name`）、监听端口、网站根目录，行内提供「编辑」「删除」操作，
右上角提供「新增站点」与「重启 Nginx」。

- **新增 / 编辑**：在弹窗中填写站点标识（域名或 IP）、监听端口、网站根目录，提交后由后端
  生成标准 `server` 块并写入 `config.d/<标识>.conf`。修改时若标识变化，文件名会同步更新。
- **删除**：二次确认后删除对应 `.conf` 文件。
- **重启 Nginx**：点击后后端先 `nginx -t` 校验配置（避免把错误配置应用上线），校验通过
  再 `nginx -s reload` 优雅重载；若 nginx 未运行则直接启动。所有变更（新增 / 修改 /
  删除）后都应点一次「重启 Nginx」使其生效。

### 站点配置注意事项

- **依赖已安装的 Nginx**：站点配置目录位于本工具安装的 `/usr/local/nginx` 之下，
  因此「站点配置」页要求先完成 Nginx 安装；未安装时页面会提示先到「安装」页安装。
- **输入校验**：站点标识（即 `server_name`）支持域名（如 `example.com`）、通配符
  （`*.example.com`）、默认占位 `_`，以及 IP 地址（IPv4 如 `192.168.1.1`、IPv6 如
  `2001:db8::1`，八位组越界会被拒绝）；端口 1~65535；根目录必须为绝对路径。非法输入会
  在前端拦截，后端也会再次校验并返回错误信息。
- **文件名安全化**：域名转文件名时剥离路径分隔符，杜绝路径穿越（如 `../etc` 不会逃逸
  出 `config.d` 目录）。同名站点不可重复新增。
- **`server_name` 默认项**：若配置中该字段为 `_`，列表显示「(默认 _)」。
