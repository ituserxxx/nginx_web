#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# build.sh - 把前端 + 后端打包成单一可执行文件，并对该文件做压缩
#
# 产物（默认）:
#   ./nginx-web        经 upx 原地压缩后的单一二进制（前后端已内嵌，可直接运行）
#   若系统无 upx，则改为生成:
#   ./nginx-web        未压缩的单一二进制
#   ./nginx-web.gz     经 gzip -9 压缩的产物
#
# 运行方式:
#   PORT=8080 ./nginx-web
#   浏览器打开 http://localhost:8080/ 即可（API 与页面由同一进程提供）
#
# 注意: 必须在 WSL / Linux 的 bash 中执行（前端依赖 Linux 原生 node 构建，
#       vite 8 的 rolldown 原生 binding 在 Windows 下 npm install 会得到
#       错误的 win32 版本，无法在 WSL 内加载）。
# ---------------------------------------------------------------------------
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_SRC="$ROOT/web/dist"
DIST_DST="$ROOT/server/dist"
BIN="$ROOT/nginx-web"

# ---------------------------------------------------------------- 输出工具
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_DIM=''; C_OFF=''
fi
info() { printf '%s[info]%s %s\n' "$C_BLUE"   "$C_OFF" "$*"; }
ok()   { printf '%s[ ok ]%s %s\n' "$C_GREEN"  "$C_OFF" "$*"; }
warn() { printf '%s[warn]%s %s\n' "$C_YELLOW" "$C_OFF" "$*"; }
err()  { printf '%s[fail]%s %s\n' "$C_RED"    "$C_OFF" "$*" >&2; }
step() { printf '%s==>%s %s\n'    "$C_DIM"    "$C_OFF" "$*"; }

# ---------------------------------------------------------------- 环境判定
case "$(uname -s 2>/dev/null || echo unknown)" in
  Linux) ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    err "检测到 Windows 环境 ($(uname -s))，本脚本必须在 WSL / Linux 的 bash 中运行"
    err "请在 WSL (Ubuntu) 终端里执行:"
    err "    cd /mnt/d/DDD/xxx/code/nginx_web && ./build.sh"
    exit 1
    ;;
  *)
    warn "未知系统 ($(uname -s))，按 Linux 继续，如遇问题请确认环境"
    ;;
esac

# ---------------------------------------------------------------- 工具链
setup_go() {
  command -v go >/dev/null 2>&1 && return 0
  local d
  for d in /usr/local/go/bin "$HOME/go/bin" /usr/lib/go/bin /usr/lib/go-*/bin /snap/bin; do
    [[ -x "$d/go" ]] && { export PATH="$PATH:$d"; return 0; }
  done
  return 1
}

NODE_BIN=""
setup_node() {
  local cands=() c
  command -v node >/dev/null 2>&1 && cands+=("$(command -v node)")
  for c in /usr/local/bin/node /usr/bin/node "$HOME"/.nvm/versions/node/*/bin/node; do
    [[ -x "$c" ]] && cands+=("$c")
  done
  # 优先 WSL 原生 node，排除 Windows 的 node.exe 与 /mnt 下互操作的版本
  for c in "${cands[@]:-}"; do
    [[ -n "$c" && "$c" != *.exe && "$c" != /mnt/* ]] && { NODE_BIN="$c"; return 0; }
  done
  for c in "${cands[@]:-}"; do
    [[ -n "$c" ]] && { NODE_BIN="$c"; warn "回退使用 Windows node: $NODE_BIN"; return 0; }
  done
  return 1
}

step "检查工具链"
if ! setup_go; then err "未找到 go，请先安装 Go 或加入 PATH (Ubuntu: sudo apt install -y golang-go)"; exit 1; fi
if ! setup_node; then err "未找到 node，请先安装 Node.js"; exit 1; fi
command -v npm >/dev/null 2>&1 || { err "未找到 npm，请安装含 npm 的 Node.js"; exit 1; }
info "go   -> $(command -v go) ($(go version 2>/dev/null | awk '{print $3}'))"
info "node -> $NODE_BIN ($("$NODE_BIN" -v 2>/dev/null))"

# ---------------------------------------------------------------- 1. 构建前端
step "构建前端 (Vite -> web/dist)"
cd "$ROOT/web"

NM="$ROOT/web/node_modules"
need_reinstall() {
  [[ -d "$NM" ]] || return 0
  [[ -f "$NM/vite/bin/vite.js" ]] || return 0
  local arch os
  arch="$(uname -m)"
  case "$arch" in x86_64|amd64) arch=x64 ;; aarch64|arm64) arch=arm64 ;; esac
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  [[ "$os" == "linux" ]] || return 1
  for b in "$NM"/@rolldown/binding-linux-"$arch"-gnu "$NM"/@rolldown/binding-linux-"$arch"-musl; do
    [[ -d "$b" ]] && return 1
  done
  return 0
}

if need_reinstall; then
  warn "前端依赖与当前平台不匹配（多半是在 Windows 下 npm install 过），将重新安装"
  rm -rf "$NM"
fi
if [[ ! -d "$NM" ]]; then
  info "首次安装前端依赖"
  npm install || { err "npm install 失败，检查网络或 npm 源"; exit 1; }
fi
npm run build || { err "前端构建失败"; exit 1; }
if [[ ! -f "$DIST_SRC/index.html" ]]; then
  err "前端构建产物缺失: $DIST_SRC/index.html"
  exit 1
fi
ok "前端构建完成: $DIST_SRC"

# ---------------------------------------------------------------- 2. 复制前端产物
step "把前端产物复制进 server/dist（将由 Go 内嵌）"
rm -rf "$DIST_DST"
cp -r "$DIST_SRC" "$DIST_DST"
ok "已复制 $(find "$DIST_DST" -type f | wc -l | tr -d ' ') 个文件到 $DIST_DST"

# ---------------------------------------------------------------- 3. 构建后端
step "构建后端 (Go, -tags embed 内嵌前端)"
cd "$ROOT/server"
if ! go build -tags embed -ldflags "-s -w" -o "$BIN" .; then
  err "后端构建失败"
  rm -rf "$DIST_DST"
  exit 1
fi
# 内嵌已完成，清理临时复制的前端产物，避免污染仓库
rm -rf "$DIST_DST"
ok "已生成单一二进制: $BIN ($(du -h "$BIN" | cut -f1))"

# ---------------------------------------------------------------- 4. 压缩
step "压缩二进制"
FINAL="$BIN"
if command -v upx >/dev/null 2>&1; then
  if upx --best --lzma "$BIN"; then
    ok "已用 upx 原地压缩: $BIN ($(du -h "$BIN" | cut -f1))"
  else
    warn "upx 压缩失败，保留未压缩二进制"
  fi
else
  warn "未找到 upx，使用 gzip 压缩为 ${BIN}.gz（如需单文件可直接运行请用 apt install upx）"
  if gzip -9 -k -f "$BIN"; then
    ok "已生成 ${BIN}.gz ($(du -h "${BIN}.gz" | cut -f1))"
    FINAL="${BIN}.gz"
  else
    warn "gzip 压缩失败，保留未压缩二进制"
  fi
fi

# ---------------------------------------------------------------- 完成
printf '\n'
ok "构建完成"
printf '  运行方式:  %sPORT=8080 %s%s\n' "$C_GREEN" "$BIN" "$C_OFF"
printf '  访问地址:  %shttp://localhost:8080/%s\n' "$C_GREEN" "$C_OFF"
printf '  产物路径:  %s%s%s\n' "$C_GREEN" "$FINAL" "$C_OFF"
