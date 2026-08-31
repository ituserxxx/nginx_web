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

- 站点 = `config.d/<标识>.conf` 里的 `server { listen; server_name; root; }` 块
- 后端 API：`GET /api/nginx/sites`（列表+nginx安装状态）、
  `POST /api/nginx/sites/create`、`POST /api/nginx/sites/update`、
  `POST /api/nginx/sites/delete`、`POST /api/nginx/reload`（先 `nginx -t` 校验再 `reload`，未运行则 start）
- 标识→文件名安全化（sanitizeSiteFile，剥离 `/` 与 `..`，防路径穿越）；同名不可重复新增
- 校验（isValidServerName）：支持域名 / 通配符(`*.example.com`) / 默认占位 `_` / IPv4(八位组 0~255 强校验) / IPv6(字符白名单，语义交 `nginx -t`)；
  端口 1~65535、根目录为绝对路径；renderSiteConf 生成、parseSiteConf 解析
- 前端左菜单「站点配置」视图：列表(文件/域名或IP/端口/根目录) + 新增/编辑弹窗 + 删除确认 + 重启按钮
- 站点配置依赖已安装的 Nginx（configDir 在 /usr/local/nginx/conf/config.d 下），未安装时页面提示

## 技术栈

- 后端：Go（server/main.go，单文件，**零第三方依赖**，标准库 net/http）
  - API：`GET /api/hello`、`GET /api/nginx`、`GET /api/nginx/available`、
    `POST /api/nginx/install`、`GET /api/nginx/install/status`、`GET /api/nginx/installs`、
    `POST /api/nginx/uninstall`、`GET /api/nginx/sites`、
    `POST /api/nginx/sites/{create,update,delete}`、`POST /api/nginx/reload`
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

## 前端改动的验证方法（Windows 上跑不了 vite）

node_modules 只有 linux 版 rolldown binding，Windows 下无法启动 dev server，
改前端后用这两招验证，都不需要启动服务：

1. require `@vue/compiler-sfc`，依次 `parse` -> `compileScript` -> `compileTemplate`
   （compileTemplate 要传 `bindingMetadata`），可查出模板语法与未定义变量。
2. 抽出 `<script setup>` 内容，用 `new Function` 注入 mock 的
   ref / computed / onMounted / fetch，直接调用组件方法断言结果。

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
