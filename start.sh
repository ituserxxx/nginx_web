#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# nginx_web 开发启动脚本（WSL2 / Linux 专用）
#
# 用法:
#   ./start.sh            启动（先干净停止旧实例，再启动前后端）
#   ./start.sh stop       停止所有服务
#   ./start.sh restart    重启
#   ./start.sh status     查看运行状态与健康检查
#
# 环境变量:
#   PORT=8080             后端端口（前端代理自动跟随）
#   VITE_PORT=5173        前端端口
#   VITE_POLL=1           强制开启文件轮询监听（/mnt 下自动开启）
#   SKIP_PORT_CLEAN=1     停止时跳过端口残留清理
#   NO_COLOR=1            关闭彩色输出
#
# 注意: 必须在 WSL / Linux 的 bash 中执行，不要在 Windows 的 Git Bash 里跑。
# ---------------------------------------------------------------------------
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="$ROOT/.run"

# 端口：用户显式指定则不做自动避让，否则冲突时自动顺延
if [[ -n "${PORT:-}" ]]; then API_PORT_EXPLICIT=1; else API_PORT_EXPLICIT=0; fi
if [[ -n "${VITE_PORT:-}" ]]; then WEB_PORT_EXPLICIT=1; else WEB_PORT_EXPLICIT=0; fi
API_PORT="${PORT:-8080}"
WEB_PORT="${VITE_PORT:-5173}"

# ---------------------------------------------------------------- 输出工具
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
  C_BLUE=$'\033[34m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_DIM=''; C_OFF=''
fi
info() { printf '%s[info]%s %s\n' "$C_BLUE"   "$C_OFF" "$*"; }
ok()   { printf '%s[ ok ]%s %s\n' "$C_GREEN"  "$C_OFF" "$*"; }
warn() { printf '%s[warn]%s %s\n' "$C_YELLOW" "$C_OFF" "$*"; }
err()  { printf '%s[fail]%s %s\n' "$C_RED"    "$C_OFF" "$*" >&2; }
step() { printf '%s==>%s %s\n'    "$C_DIM"    "$C_OFF" "$*"; }

mkdir -p "$RUN_DIR"

# ---------------------------------------------------------------- 环境判定
in_wsl() { grep -qi microsoft /proc/version 2>/dev/null; }
is_wsl2() { grep -qi 'WSL2\|microsoft-standard' /proc/version 2>/dev/null; }

# 拒绝在 Windows 的 Git Bash / MSYS / Cygwin 中运行
case "$(uname -s 2>/dev/null || echo unknown)" in
  Linux) ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    err "检测到 Windows 环境 ($(uname -s))，本脚本必须在 WSL / Linux 的 bash 中运行"
    err "请在 WSL (Ubuntu) 终端里执行:"
    err "    cd /mnt/d/DDD/xxx/code/nginx_web && ./start.sh"
    exit 1
    ;;
  *)
    warn "未知系统 ($(uname -s))，按 Linux 继续，如遇问题请确认环境"
    ;;
esac

# ------------------------------------------------- 端口占用探测（多级回退）
pids_on_port() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -lptnH "sport = :${port}" 2>/dev/null \
      | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u || true
  elif command -v lsof >/dev/null 2>&1; then
    lsof -ti:"${port}" 2>/dev/null || true
  elif command -v fuser >/dev/null 2>&1; then
    fuser "${port}/tcp" 2>/dev/null | tr -s ' ' '\n' | grep -E '^[0-9]+$' || true
  fi
}

# WSL2 的 localhostForwarding 会让 Windows 侧已占用的端口在 WSL 内也绑不上。
# 一次性拉取 Windows 侧全部 LISTENING 端口并缓存，避免反复调用 netstat.exe。
WIN_PORTS=""
win_load_ports() {
  in_wsl || return 0
  command -v netstat.exe >/dev/null 2>&1 || return 0
  WIN_PORTS="$(netstat.exe -ano 2>/dev/null | tr -d '\r' \
    | awk '$1=="TCP" && $4=="LISTENING" { n=split($2,a,":"); print a[n] }' \
    | sort -un || true)"
}
win_port_busy() { printf '%s\n' "$WIN_PORTS" | grep -qx "$1"; }
win_pids_on_port() {
  in_wsl || return 0
  command -v netstat.exe >/dev/null 2>&1 || return 0
  netstat.exe -ano 2>/dev/null | tr -d '\r' \
    | awk -v p=":${1}\$" '$1=="TCP" && $2 ~ p && $4=="LISTENING" { print $5 }' \
    | sort -u || true
}

# 端口空闲判定
port_free() {
  local p="$1"
  [[ -z "$(pids_on_port "$p")" ]] || return 1
  win_port_busy "$p" && return 1
  return 0
}

# 从起始端口开始寻找可用端口（用户显式指定则不顺延）
resolve_port() {
  local start="$1" explicit="$2" i p
  for ((i = 0; i < 25; i++)); do
    p=$((start + i))
    if port_free "$p"; then printf '%s' "$p"; return 0; fi
    if [[ "$explicit" == "1" ]]; then
      err "端口 $p 已被占用，且由你显式指定，不做自动避让"
      return 1
    fi
  done
  err "在 ${start} 起 25 个端口内未找到可用端口"
  return 1
}

# ---------------------------------------------------------------- 进程管理
kill_pid() {
  local pid="$1" i
  [[ -n "$pid" && "$pid" =~ ^[0-9]+$ ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < 25; i++)); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
}

# 停止服务：优先按进程组杀（setsid 启动时 PID 即 PGID），可清掉 esbuild 子进程
kill_pidfile() {
  local name="$1" pf="$RUN_DIR/$1.pid" pid i
  [[ -f "$pf" ]] || return 0
  pid="$(cat "$pf" 2>/dev/null || true)"
  rm -f "$pf"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < 25; i++)); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  warn "$name (pid=$pid) 未响应 SIGTERM，强制结束"
  kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
}

# 以后台进程组方式启动命令，并把真实 PID 写进 PID 文件
launch() {
  local name="$1" pidf="$RUN_DIR/$1.pid" logf="$RUN_DIR/$1.log" i
  shift
  rm -f "$pidf"
  : > "$logf"
  if command -v setsid >/dev/null 2>&1; then
    setsid bash -c 'pidf="$1"; shift; echo $$ >"$pidf"; exec "$@"' \
      _ "$pidf" "$@" >>"$logf" 2>&1 </dev/null &
  else
    bash -c 'pidf="$1"; shift; echo $$ >"$pidf"; exec "$@"' \
      _ "$pidf" "$@" >>"$logf" 2>&1 </dev/null &
  fi
  for ((i = 0; i < 60; i++)); do
    [[ -s "$pidf" ]] && return 0
    sleep 0.1
  done
  return 1
}

alive() {
  local pf="$RUN_DIR/$1.pid" pid
  [[ -f "$pf" ]] || return 1
  pid="$(cat "$pf" 2>/dev/null || true)"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  kill -0 "$pid" 2>/dev/null
}

stop_services() {
  kill_pidfile backend  || true
  kill_pidfile frontend || true

  if [[ "${SKIP_PORT_CLEAN:-0}" != "1" ]]; then
    local p pid
    for p in "$API_PORT" "$WEB_PORT"; do
      for pid in $(pids_on_port "$p"); do
        warn "清理端口 $p 上的残留进程 (pid=$pid)"
        kill_pid "$pid"
      done
    done
  fi

  # 兜底：清掉本项目目录下遗留的 vite / esbuild 子进程
  if command -v pgrep >/dev/null 2>&1; then
    local pid
    for pid in $(pgrep -f "$ROOT/web/node_modules" 2>/dev/null || true); do
      kill_pid "$pid"
    done
  fi
  sleep 0.3
}

# ---------------------------------------------------------------- 工具链
setup_go() {
  if command -v go >/dev/null 2>&1; then return 0; fi
  local d
  for d in /usr/local/go/bin /usr/lib/go/bin /usr/lib/go-*/bin /snap/bin \
           "$HOME/go/bin" "$HOME/.local/go/bin" /usr/local/go-*/bin; do
    if [[ -x "$d/go" ]]; then
      export PATH="$PATH:$d"
      return 0
    fi
  done
  export PATH="$PATH:$HOME/go/bin"
  command -v go >/dev/null 2>&1
}

NODE_BIN=""
setup_node() {
  local cands=() c
  command -v node >/dev/null 2>&1 && cands+=("$(command -v node)")
  if [[ -d "$HOME/.nvm/versions/node" ]]; then
    for c in "$HOME"/.nvm/versions/node/*/bin/node; do
      [[ -x "$c" ]] && cands+=("$c")
    done
  fi
  for c in /usr/local/bin/node /usr/bin/node /snap/bin/node; do
    [[ -x "$c" ]] && cands+=("$c")
  done
  # 优先 WSL 原生 node，排除 /mnt 下的 Windows node.exe
  for c in "${cands[@]:-}"; do
    [[ -n "$c" ]] || continue
    [[ "$c" == *.exe || "$c" == /mnt/* ]] && continue
    NODE_BIN="$c"
    return 0
  done
  # 回退到 Windows node（互操作）：能跑但慢，且文件监听基本失效
  for c in "${cands[@]:-}"; do
    [[ -n "$c" ]] || continue
    NODE_BIN="$c"
    warn "未找到 WSL 原生 node，回退使用 Windows node: $NODE_BIN"
    warn "建议安装 WSL 原生 Node.js（nvm）：性能更好且文件监听可用"
    return 0
  done
  return 1
}

# ---------------------------------------------------------------- 健康检查
wait_http() {
  local url="$1" tries="${2:-40}" i
  command -v curl >/dev/null 2>&1 || { sleep 2; return 0; }
  for ((i = 0; i < tries; i++)); do
    if curl -fsS -m 2 "$url" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  return 1
}

# 判断前端依赖是否需要（重新）安装。返回 0 = 需要安装，1 = 无需安装。
# vite 8 依赖 rolldown 的平台原生二进制：在 Windows 下 npm install 得到的
# node_modules 只带 win32 binding，WSL 里加载会报 "Cannot find native binding"，
# 因此不能只看 vite.js 是否存在，必须校验 binding 与当前平台是否匹配。
need_reinstall() {
  local nm="$ROOT/web/node_modules"
  [[ -d "$nm" ]] || return 0
  [[ -f "$nm/vite/bin/vite.js" ]] || return 0

  local arch os b
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  arch=x64 ;;
    aarch64|arm64) arch=arm64 ;;
  esac
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"

  if [[ "$os" == "linux" ]]; then
    for b in "$nm"/@rolldown/binding-linux-"$arch"-gnu \
             "$nm"/@rolldown/binding-linux-"$arch"-musl; do
      [[ -d "$b" ]] && return 1
    done
    return 0
  fi
  return 1
}

# ---------------------------------------------------------------- 启动流程
start_backend() {
  step "构建后端 (Go)"
  if ! ( cd "$ROOT/server" && go build -o "$RUN_DIR/server" . ); then
    err "Go 构建失败，请检查 server/ 代码或 go 环境"
    return 1
  fi
  ok "后端构建完成"

  launch backend env PORT="$API_PORT" "$RUN_DIR/server" \
    || { err "后端进程启动失败，查看日志: $RUN_DIR/backend.log"; return 1; }

  if wait_http "http://127.0.0.1:${API_PORT}/api/hello" 40; then
    ok "后端已就绪  http://localhost:${API_PORT}/api/hello"
  else
    err "后端未在 20 秒内响应，日志: $RUN_DIR/backend.log"
    tail -n 20 "$RUN_DIR/backend.log" >&2 || true
    return 1
  fi
}

start_frontend() {
  step "启动前端 (Vite)"
  if need_reinstall; then
    local nm="$ROOT/web/node_modules"
    if [[ -d "$nm" ]]; then
      warn "前端依赖与当前平台不匹配（多半是在 Windows 下 npm install 过）"
      warn "将删除 $nm 并重新安装"
      rm -rf "$nm"
    else
      info "首次运行，安装前端依赖"
    fi
    if [[ "$ROOT" == /mnt/* ]]; then
      info "项目位于 /mnt 挂载目录，npm install 会明显变慢，请耐心等待"
    fi
    ( cd "$ROOT/web" && npm install ) \
      || { err "npm install 失败，检查网络或 npm 源"; return 1; }
    ok "前端依赖安装完成"
  fi

  # drvfs (/mnt/*) 不支持 inotify，必须开启轮询，否则改代码不触发热更新
  local poll="${VITE_POLL:-}"
  if [[ "$ROOT" == /mnt/* && "$poll" != "0" ]]; then
    poll=1
    warn "项目位于 /mnt 挂载目录，已自动开启文件轮询监听（HMR 会稍慢）"
    warn "追求性能可把项目移到 WSL 原生目录，例如: cp -r $ROOT ~/nginx_web"
  fi

  ( cd "$ROOT/web" && \
    API_PORT="$API_PORT" VITE_POLL="$poll" \
    launch frontend "$NODE_BIN" node_modules/vite/bin/vite.js \
      --port "$WEB_PORT" --strictPort ) \
    || { err "前端进程启动失败，查看日志: $RUN_DIR/frontend.log"; return 1; }

  if wait_http "http://127.0.0.1:${WEB_PORT}/" 40; then
    ok "前端已就绪  http://localhost:${WEB_PORT}"
  else
    err "前端未在 20 秒内响应，日志: $RUN_DIR/frontend.log"
    tail -n 20 "$RUN_DIR/frontend.log" >&2 || true
    return 1
  fi
}

do_start() {
  info "项目根目录: $ROOT"
  if in_wsl; then
    if is_wsl2; then
      info "运行环境: WSL2 ($(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-Ubuntu}" || echo Ubuntu))"
    else
      warn "运行环境: WSL1（端口与文件系统行为与 WSL2 有差异）"
    fi
  else
    info "运行环境: $(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-Linux}" || echo Linux)"
  fi

  # 先探测 Windows 侧端口占用（WSL2 关键坑）
  win_load_ports

  step "停止旧实例"
  stop_services
  ok "已清理旧实例"

  # 端口避让
  local resolved
  if ! resolved="$(resolve_port "$API_PORT" "$API_PORT_EXPLICIT")"; then
    local wp
    wp="$(win_pids_on_port "$API_PORT" || true)"
    if [[ -n "$wp" ]]; then
      err "端口 $API_PORT 被 Windows 侧进程占用 (PID: $(echo $wp | tr '\n' ' '))"
      err "WSL2 的 localhostForwarding 会导致 WSL 内无法绑定该端口。可选方案:"
      err "  1) 换端口启动:      PORT=8090 ./start.sh"
      err "  2) 关闭那个 Windows 程序后重试"
      err "  3) 在 Windows 的 %USERPROFILE%\\.wslconfig 中加入 [wsl2] 与 localhostForwarding=false 后执行 wsl --shutdown"
    fi
    return 1
  fi
  if [[ "$resolved" != "$API_PORT" ]]; then
    warn "端口 $API_PORT 不可用，自动改用 $resolved"
  fi
  API_PORT="$resolved"

  resolved="$(resolve_port "$WEB_PORT" "$WEB_PORT_EXPLICIT")" \
    || { err "前端端口 $WEB_PORT 不可用"; return 1; }
  if [[ "$resolved" != "$WEB_PORT" ]]; then
    warn "前端端口 $WEB_PORT 不可用，自动改用 $resolved"
  fi
  WEB_PORT="$resolved"

  # 工具链
  step "检查工具链"
  if ! setup_go; then
    err "未找到 go，请先安装 Go 或把它加入 PATH"
    err "  Ubuntu: sudo apt install -y golang-go"
    return 1
  fi
  info "go    -> $(command -v go) ($(go version 2>/dev/null | awk '{print $3}' || echo unknown))"

  if ! setup_node; then
    err "未找到 node，请先安装 Node.js"
    err "  推荐: curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.39.7/install.sh | bash"
    return 1
  fi
  info "node  -> $NODE_BIN ($("$NODE_BIN" -v 2>/dev/null || echo unknown))"

  if ! command -v npm >/dev/null 2>&1; then
    err "未找到 npm，请安装 Node.js（含 npm）"
    return 1
  fi

  start_backend  || return 1
  start_frontend || return 1

  printf '\n'
  ok "全部服务已启动"
  printf '   前端       %shttp://localhost:%s%s\n'   "$C_GREEN" "$WEB_PORT" "$C_OFF"
  printf '   后端       %shttp://localhost:%s/api/hello%s\n' "$C_GREEN" "$API_PORT" "$C_OFF"
  printf '   后端日志   tail -f .run/backend.log\n'
  printf '   前端日志   tail -f .run/frontend.log\n'
  printf '   停止服务   ./start.sh stop\n'
  printf '\n'
  if [[ -z "$(command -v nginx 2>/dev/null || true)" ]]; then
    info "当前环境未安装 nginx，页面会显示“未检测到 Nginx 安装”，属预期结果"
    info "需要真实数据可安装: sudo apt install -y nginx"
  fi
}

do_status() {
  local any=0
  printf '%s服务状态%s\n' "$C_BLUE" "$C_OFF"
  for s in backend frontend; do
    if alive "$s"; then
      local pid
      pid="$(cat "$RUN_DIR/$s.pid")"
      printf '  %-9s %s运行中%s (pid=%s)\n' "$s" "$C_GREEN" "$C_OFF" "$pid"
      any=1
    else
      printf '  %-9s %s已停止%s\n' "$s" "$C_YELLOW" "$C_OFF"
    fi
  done
  printf '\n%s端口与健康%s\n' "$C_BLUE" "$C_OFF"
  local p
  for p in "$API_PORT" "$WEB_PORT"; do
    local pid
    pid="$(pids_on_port "$p" | head -1 || true)"
    if [[ -n "$pid" ]]; then
      printf '  %-6s 被占用 (pid=%s)\n' "$p" "$pid"
    else
      printf '  %-6s 空闲\n' "$p"
    fi
  done
  if command -v curl >/dev/null 2>&1; then
    curl -fsS -m 3 "http://127.0.0.1:${API_PORT}/api/hello" >/dev/null 2>&1 \
      && printf '  /api/hello  %s正常%s\n' "$C_GREEN" "$C_OFF" \
      || printf '  /api/hello  %s无响应%s\n' "$C_YELLOW" "$C_OFF"
  fi
  return 0
}

# ---------------------------------------------------------------- 入口
case "${1:-start}" in
  start)   do_start ;;
  stop)    stop_services; ok "已停止所有服务" ;;
  restart) stop_services; ok "已停止所有服务"; do_start ;;
  status)  do_status ;;
  -h|--help|help)
    awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "${BASH_SOURCE[0]}"
    ;;
  *)
    err "未知命令: $1"
    err "用法: $0 [start|stop|restart|status]"
    exit 1
    ;;
esac
