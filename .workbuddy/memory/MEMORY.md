# nginx_web 项目长期约定

## 项目定位

nginx 检测控制台：检测机器已安装的 nginx（版本 + 执行路径）、查询可安装版本、
对未安装版本执行源码编译安装，并能在「已安装」行卸载本工具编译安装的版本。
另含「站点配置」页（左侧菜单切换），管理 config.d 下的站点（域名新增/修改/删除 + 重启 Nginx）。

安装相关约定（详见 README）：
- 源码下载解压在 /data/soft，编译安装到 /usr/local/nginx，软链到 /usr/bin/nginx
- 异步任务制：POST /api/nginx/install 返回 taskId，前端轮询 install/status
- 卸载：POST /api/nginx/uninstall（同样返回 taskId，复用 install/status 轮询）；
  停止 nginx → 删 /usr/bin/nginx 软链 → 删 /usr/local/nginx 整目录 → 删源码目录
- 同一时刻只允许一个任务（安装与卸载互斥）；需 root；80 端口被占会失败
- 卸载只针对固定前缀 /usr/local/nginx，不清理包管理器装的 /usr/sbin/nginx
  （但 pkill 兜底会把它一并停掉）
- nginx.conf 的 config.d include 由程序写入（用户原步骤里的 vim 无法自动化）
- 安装记录持久化在 `/usr/local/nginx/nginx-web-install.json`（同版本覆盖、按时间倒序），
  由 `GET /api/nginx/installs` 读取；含配置路径、执行目录、configure 参数等
- 安装前缀/源码目录是**变量**（installPrefix/sourceDir），默认 /usr/local/nginx、/data/soft，
  单元测试中临时重定向到临时目录以验证卸载范围，改动此处记得同步测试

## 站点配置功能（config.d 管理）

- 站点 = `config.d/<标识>.conf` 里的 `server { listen; server_name; [ssl?] [root|proxy_pass] }` 块
- 后端 API：`GET /api/nginx/sites`（列表+nginx安装状态）、
  `POST /api/nginx/sites/create`、`POST /api/nginx/sites/update`、
  `POST /api/nginx/sites/delete`、`POST /api/nginx/reload`（先 `nginx -t` 校验再 `reload`，未运行则 start）
- 站点支持两种托管模式：**静态**（填 root → `try_files`）、**反向代理**（填 proxyPort →
  `proxy_pass http://127.0.0.1:<port>` + 代理头，免填 root）；可勾选 **HTTPS**（→ `listen <port> ssl;`
  + `ssl_certificate`/`ssl_certificate_key`，需证书/私钥绝对路径）
- siteConfig/siteInput 字段：`domain, listen, root, proxyPort, ssl, cert, key`；
  renderSiteConf(siteInput) 生成、parseSiteConf(content) siteConfig 解析（往返守恒）
- 标识→文件名安全化（sanitizeSiteFile，剥离 `/` 与 `..`，防路径穿越）；同名不可重复新增
- 校验（isValidServerName）：支持域名 / 通配符(`*.example.com`) / 默认占位 `_` / IPv4(八位组 0~255 强校验) / IPv6(字符白名单，语义交 `nginx -t`)；
  监听端口 1~65535、代理端口(可选)1~65535、根目录为绝对路径；启用 HTTPS 时 cert/key 必填且为绝对路径；
  **反向代理模式免填 root**（proxyPort>0 即进入代理模式）
- 前端左菜单「站点配置」视图：列表(文件/域名或IP/端口/根目录/代理端口/HTTPS) + 新增/编辑弹窗 + 删除确认 + 重启按钮
- 站点配置依赖已安装的 Nginx（configDir 在 /usr/local/nginx/conf/config.d 下），未安装时页面提示

## 证书上传（SSL）

- `POST /api/nginx/sites/cert`：`multipart/form-data`，字段 `domain` + `file`，上限 32MB。
  按域名归置到 `conf/ssl/<站点标识>/`，解压后识别 cert/key 并返回绝对路径 + 文件清单，
  前端自动回填表单（用户再点「保存」才写入 nginx 配置 —— 不做隐式落盘）
- 归置目录 `sslBaseDir()` = `<installPrefix>/conf/ssl`；`certDirName()` 做安全化
  （`/` `\` `:` → `_`，再取 Base，再去前导点，空则回退 `site`）
- 解压 `extractCertArchive` 按后缀分派：`.zip` / `.tar.gz` / `.tgz`（gzip+tar）/ `.tar`；
  其余按单文件直写。重复上传先 `RemoveAll` 清空该目录
- **识别按内容而非扩展名**：`classifyPEM` 含 `BEGIN CERTIFICATE` → 证书，含 `PRIVATE KEY`
  → 私钥（覆盖 RSA/EC 变体）；内容判不出才按扩展名兜底（cert: pem/crt/cer，key: key）。
  递归 `filepath.Walk`，多个时取排序靠前者保证稳定
- **zip-slip 防护 `safeJoin`**：不要用 `filepath.IsAbs` 判定（Windows 语义差异会漏判），
  改为先按斜杠判 `HasPrefix("/")` 与盘符（`x[1]==':'`），再 Clean 后判 `..`，
  最后用 `filepath.Abs` 前缀做兜底。单测覆盖了 `../` 与绝对路径两种攻击
- 前端：`certMode` 切「上传 / 手动填写」，`a-upload-dragger` + `customRequest`（要带 domain，
  故不能用默认提交）；`customUpload` **返回 Promise** 以便调用方 await（a-upload 忽略返回值）

## 技术栈

- 后端：Go（server/main.go，单文件，**零第三方依赖**，标准库 net/http）
  - API：`GET /api/hello`、`GET /api/nginx`、`GET /api/nginx/available`、
    `POST /api/nginx/install`、`GET /api/nginx/install/status`、`GET /api/nginx/installs`、
    `POST /api/nginx/uninstall`、`GET /api/nginx/sites`、
    `POST /api/nginx/sites/{create,update,delete,cert}`、`POST /api/nginx/reload`
  - 非 Linux 系统（runtime.GOOS 判定）返回 `supported:false`，前端据此提示
  - 含单元测试 `main_test.go`（解析函数、版本注入防护、config.d 写入、卸载范围、站点配置），改后端后应跑 go test
- 前端：Vue 3.5（script setup）+ Vite 8.2 + ant-design-vue 4.2
  - 源码极简：src/ 下只有 App.vue、main.js、style.css，无路由无状态管理
    （用 `currentMenu` ref 在单文件内切换「安装」/「站点配置」两个视图，未引入 vue-router）
  - 左侧 `a-layout-sider` + `a-menu`（安装 / 站点配置），当前安装页即「安装」视图
  - Vite dev 把 /api 代理到后端
  - 用 `import { message as antdMessage } from 'ant-design-vue'` 做操作反馈（与 header 的 message ref 区分）
- 两个接口的版本字符串格式不同，比对前必须归一化：
  `nginx/1.24.0` 与 `1.24.0-2ubuntu7.1` / `1.24.0-r6` / `1:1.24.0-1`
  都要归一到 `1.24.0`。正则需含冒号 `/\d[\w.:+-]*/`，否则 epoch 版被截断。

## 单文件构建（embed 机制）

- `build.sh`（WSL/Linux 专用，拒绝 Windows Git Bash）把前端 `web/dist` 经 `//go:embed`
  编译进后端二进制，产出单一可执行 `nginx-web`，再用 upx（优先）/ gzip 压缩。
- 构建标签 `embed` 控制是否内嵌：`server/embed.go`（`//go:build embed`，`//go:embed dist`）
  与 `server/noembed.go`（`//go:build !embed`，空 `webrootFS` + `embedWebroot=false`）。
- **刻意不让 `go test` 与 `./start.sh` 带 embed 标签** —— 它们无需前端构建产物即可编译；
  开发模式下后端只暴露 `/api`，前端仍由 Vite 单独提供。仅当 `build.sh` 用 `-tags embed`
  时才内嵌前端，并由 `registerWebroot(mux)` 注册 `/` 兜底路由（SPA 回退到 index.html，
  同时把 `/api*` 未知路径置 404，不影响既有接口）。
- `registerWebroot` 的 SPA 回退**必须**直接 `fs.ReadFile(sub,"index.html")` 写出 `text/html`，
  不能把 `r.URL.Path` 改成 `/index.html` 交给 `http.FileServer` —— 后者会把
  `/index.html` 301 重定向到 `/`，使不跟随重定向的客户端拿到空响应。
- `build.sh` 构建前 `rm -rf server/dist` 再 `cp -r web/dist server/dist`，结束后清理；
  `server/dist` 已 gitignore（非 embed 构建时目录不应存在）。

## 前端改动的验证方法（Windows 上跑不了 vite）

node_modules 只有 linux 版 rolldown binding，Windows 下无法启动 dev server，
改前端后用这两招验证，都不需要启动服务：

1. require `@vue/compiler-sfc`，`parse` 后直接 `compileScript(descriptor, { id, inlineTemplate: true })`，
   检查 `script.errors` 即可同时验证脚本与模板（**务必 inlineTemplate:true**，与 Vite 构建一致）。
   ⚠️ 不要用 `compileTemplate` 单独编译：分离模式下会把 `v-model="siteForm.x"`（reactive 成员）
   误报 "v-model cannot be used on a const binding"，导致假失败。inline 模式能正确识别为可写。
   片段/字段断言直接对 `src` 字符串做 includes 检查即可。
2. 抽出 `<script setup>` 内容（过滤掉 `import` 行），用 `new Function` 注入 mock 的
   ref / reactive / computed / onMounted / fetch / FormData / antdMessage，
   再从函数体 `return` 出要断言的方法与状态。
   ⚠️ 不要注入与组件内同名的变量（如 `siteForm`），会报
   "Identifier 'siteForm' has already been declared"；
   让组件自己声明，从返回值里取。
   ⚠️ 若组件方法内部起了异步链而不 `return` Promise，`await` 会提前结束导致断言取到旧值。

测试脚本写成 .cjs 文件执行，不要用 `node -e`（单引号会与 shell 冲突）。详见当日日志。

## 运行约束（重要）

- **必须在 WSL / Linux 的 bash 中运行**，脚本会拦截 Windows Git Bash 并报错退出。
- 用户环境：Windows 宿主机 + WSL2 Ubuntu 22.04，项目在 `/mnt/d/DDD/xxx/code/nginx_web`。
- WSL2 的 localhostForwarding 会让 Windows 侧已监听的端口在 WSL 内也 bind 失败。
  遇到 `address already in use` 但 WSL 内 `ss` 查不到占用者，就是 Windows 侧占用。
- `web/node_modules` 只含 linux-x64 的 rolldown binding，无 win32 版本 ——
  前端**只能**用 WSL 原生 node 跑，Windows node.exe 会报 Cannot find native binding。
- 项目在 `/mnt` 下，drvfs 不支持 inotify，必须开轮询监听（脚本自动处理）。

## 启动

```
./start.sh            # 启动（先停止旧实例）
./start.sh stop|restart|status
PORT=8090 ./start.sh  # 换后端端口，前端代理自动跟随
```

日志在 `.run/backend.log` 与 `.run/frontend.log`（.run/ 已 gitignore）。
换行符由 `.gitattributes` 锁定为 LF，覆盖仓库的 core.autocrlf=true。

## 用户协作偏好

- 用户会明确指定「只需要你改代码，我自己执行」——此时不要启动服务，
  只做静态校验（bash -n、go vet/build）并把结论说清楚。

## 登录鉴权（gating 安装/配置页）

- 密码：`APP_PASSWORD`，优先级 环境变量 > `.env`（项目根）> 默认 `admin`；改了需重启后端。
- 会话：登录成功后后端下发 HttpOnly Cookie（`nginx_web_session`，关闭浏览器失效），内存态
  `sessions` 映射，进程重启即全部登出；密码用 `subtle.ConstantTimeCompare` 比对。
- 鉴权：`requireAuth` 中间件保护除公开接口（`/api/hello`、`/api/login`、`/api/me`、
  `/api/logout`）之外的所有 `/api/nginx*`，未带有效 Cookie 返回 401。
- 前端：`onMounted` 先 `GET /api/me` 决定显示登录页还是主应用；`detect`/`loadSites` 遇 401
  经 `handleUnauthorized` 退回登录页。单文件部署与 Vite 代理下 fetch 同源，默认带 Cookie。
- 真正的 `.env` 已 gitignore；模板见 `.env.example`。
