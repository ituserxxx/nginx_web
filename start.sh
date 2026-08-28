#!/usr/bin/env bash
# 调试启动脚本：每次启动先杀掉上一次的前后端进程，再重新启动
# 用法: ./start.sh        启动（先杀掉上一次的实例）
#       ./start.sh stop   停止所有服务
# （在 Linux / Ubuntu / WSL 的 bash 中运行）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="$ROOT/.run"
mkdir -p "$RUN_DIR"

# 按 PID 文件停止指定服务
kill_by_pidfile() {
  local name="$1" pidfile="$RUN_DIR/$1.pid" pid
  [[ -f "$pidfile" ]] || return 0
  pid="$(cat "$pidfile" 2>/dev/null || true)"
  rm -f "$pidfile"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    echo "停止 $name (pid=$pid)"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 15); do
      kill -0 "$pid" 2>/dev/null || return 0
      sleep 0.2
    done
    kill -9 "$pid" 2>/dev/null || true
  fi
}

# 兜底：释放项目占用的端口（清理 PID 文件丢失后残留的进程）
kill_port() {
  local port="$1"
  if command -v fuser >/dev/null 2>&1; then
    fuser -k "$port/tcp" >/dev/null 2>&1 || true
  fi
}

stop_services() {
  kill_by_pidfile backend
  kill_by_pidfile frontend
  kill_port 8080
  kill_port 5173
  sleep 0.5
}

if [[ "${1:-}" == "stop" ]]; then
  stop_services
  echo "已停止所有服务"
  exit 0
fi

# 1. 停止上一次的实例
stop_services

# 2. 构建并启动后端
# 非交互式 shell 可能没有 go 的 PATH，补充常见安装位置
if ! command -v go >/dev/null 2>&1; then
  for d in /usr/local/go/bin /usr/lib/go/bin /snap/bin; do
    [[ -x "$d/go" ]] && export PATH="$PATH:$d" && break
  done
fi
command -v go >/dev/null 2>&1 || { echo "错误: 未找到 go，请先安装"; exit 1; }

echo "[1/2] 构建后端..."
(cd "$ROOT/server" && go build -o "$RUN_DIR/server" .)
nohup "$RUN_DIR/server" > "$RUN_DIR/backend.log" 2>&1 &
echo $! > "$RUN_DIR/backend.pid"
echo "后端已启动 (pid=$(cat "$RUN_DIR/backend.pid"))  http://localhost:8080  日志: .run/backend.log"

# 3. 启动前端（直接用 node 跑 vite，保证 PID 可控）
# 优先使用 Linux node；WSL 没有装 node 时回退到 Windows 的 node.exe（走 WSL 互操作）
NODE_BIN="$(command -v node || command -v node.exe || true)"
[[ -n "$NODE_BIN" ]] || { echo "错误: 未找到 node，请安装 Node.js"; exit 1; }

if [[ ! -d "$ROOT/web/node_modules" ]]; then
  echo "安装前端依赖..."
  (cd "$ROOT/web" && npm install)
fi
(cd "$ROOT/web" && nohup "$NODE_BIN" node_modules/vite/bin/vite.js > "$RUN_DIR/frontend.log" 2>&1 &
 echo $! > "$RUN_DIR/frontend.pid")
echo "前端已启动 (pid=$(cat "$RUN_DIR/frontend.pid"))  http://localhost:5173  日志: .run/frontend.log"
