package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// nginx 检测与可安装版本查询功能仅支持 Linux（含 Ubuntu）系统
func isLinux() bool {
	return runtime.GOOS == "linux"
}

type nginxInstance struct {
	Version string `json:"version"`
	Path    string `json:"path"`
}

type nginxInfo struct {
	Supported bool            `json:"supported"`
	Installed bool            `json:"installed"`
	Instances []nginxInstance `json:"instances"`
}

type nginxAvailable struct {
	Supported bool     `json:"supported"`
	Manager   string   `json:"manager"`
	Versions  []string `json:"versions"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"message": "Hello, World!"})
}

// ---------------------------------------------------------------- 登录鉴权
// 访问安装与配置页面前需先登录。密码来自 .env 的 APP_PASSWORD，未配置时回退默认 admin。
// 鉴权采用内存态会话令牌（HttpOnly Cookie），满足本工具“单实例、轻量管控”的定位；
// 不引入外部依赖，也不做持久化（进程重启即视为全部登出，符合运维工具习惯）。

// appPassword 登录密码，由 .env 的 APP_PASSWORD 或环境变量决定，缺省为 admin
var appPassword = "admin"

// sessionCookieName Cookie 名称
const sessionCookieName = "nginx_web_session"

// sessions 存放已签发的有效会话令牌，配合 mutex 并发安全
var (
	sessionMu sync.Mutex
	sessions  = make(map[string]time.Time)
)

// parseDotEnv 解析 .env 文本（不依赖第三方库）：忽略空行与 # 注释，按首个 = 切分，
// 去除值两侧的单/双引号。供 loadDotEnv 复用，也便于单元测试。
func parseDotEnv(data string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		val = strings.Trim(val, "`\"'")
		if key != "" {
			m[key] = val
		}
	}
	return m
}

// loadDotEnv 读取工作目录下的 .env 文件；文件不存在或读取失败时返回空 map
func loadDotEnv() map[string]string {
	data, err := os.ReadFile(".env")
	if err != nil {
		return map[string]string{}
	}
	return parseDotEnv(string(data))
}

// initPassword 确定登录密码优先级：环境变量 APP_PASSWORD > .env 的 APP_PASSWORD > 默认 admin
func initPassword() {
	if p := os.Getenv("APP_PASSWORD"); p != "" {
		appPassword = p
		return
	}
	if p := loadDotEnv()["APP_PASSWORD"]; p != "" {
		appPassword = p
	}
}

// newSessionToken 生成随机会话令牌
func newSessionToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// loginHandler 校验密码并下发会话 Cookie
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败"})
		return
	}
	// 用恒定时间比较，避免密码比对侧信道
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(appPassword)) != 1 {
		writeJSON(w, map[string]string{"error": "密码错误"})
		return
	}
	token := newSessionToken()
	sessionMu.Lock()
	sessions[token] = time.Now()
	sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   0, // 会话级 Cookie，关闭浏览器即失效
	})
	writeJSON(w, map[string]any{"ok": true})
}

// validSession 依据请求中的会话 Cookie 判断是否为有效登录态
func validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	sessionMu.Lock()
	_, ok := sessions[c.Value]
	sessionMu.Unlock()
	return ok
}

// meHandler 返回当前登录态，供前端初始化时判断
func meHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 GET"})
		return
	}
	writeJSON(w, map[string]any{"authenticated": validSession(r)})
}

// logoutHandler 注销当前会话并清除 Cookie
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		sessionMu.Lock()
		delete(sessions, c.Value)
		sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, map[string]any{"ok": true})
}

// requireAuth 鉴权中间件：除公开接口（健康检查 / 登录 / 身份校验 / 登出）外，
// 所有 /api/nginx* 接口必须携带有效会话 Cookie，否则返回 401。
// 静态页面（SPA）在单文件构建下由 / 兜底返回，此处不拦截，
// 由前端根据 /api/me 决定是否展示登录页，数据接口则全部受保护。
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hello", "/api/login", "/api/me", "/api/logout":
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/nginx") {
			if !validSession(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "未登录或登录已失效"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Linux 上常见的 nginx 安装路径
var nginxCandidates = []string{
	"/usr/sbin/nginx",
	"/usr/bin/nginx",
	"/usr/local/sbin/nginx",
	"/usr/local/nginx/sbin/nginx",
	"/opt/nginx/sbin/nginx",
}

// findNginxPaths 先在 PATH 中查找，再搜索常见安装目录，按路径去重
func findNginxPaths() []string {
	var paths []string
	seen := make(map[string]bool)
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if p, err := exec.LookPath("nginx"); err == nil {
		add(p)
	}
	for _, p := range nginxCandidates {
		add(p)
	}
	return paths
}

// nginxVersion 执行 nginx -v 获取版本（nginx 将版本信息输出到 stderr）
func nginxVersion(path string) string {
	out, err := exec.Command(path, "-v").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	v := strings.TrimSpace(string(out))
	return strings.TrimPrefix(v, "nginx version: ")
}

func nginxHandler(w http.ResponseWriter, r *http.Request) {
	if !isLinux() {
		writeJSON(w, nginxInfo{Instances: []nginxInstance{}})
		return
	}
	paths := findNginxPaths()
	info := nginxInfo{Supported: true, Installed: len(paths) > 0, Instances: []nginxInstance{}}
	for _, p := range paths {
		info.Instances = append(info.Instances, nginxInstance{
			Version: nginxVersion(p),
			Path:    p,
		})
	}
	writeJSON(w, info)
}

// runCmd 执行命令并返回输出，超时自动终止（yum/dnf 可能访问网络，耗时较长）
func runCmd(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func dedupe(list []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range list {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// parseAptMadison 解析 apt-cache madison 的输出，行格式：包名 | 版本 | 来源仓库
// 同一版本常同时出现在 updates 与 security 两个源，需去重。
func parseAptMadison(out string) []string {
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "nginx" && fields[1] == "|" {
			versions = append(versions, fields[2])
		}
	}
	return dedupe(versions)
}

// aptVersions 查询 apt 源中可安装的 nginx 版本。
// 注意：Ubuntu 官方源只维护一个大版本（如 22.04 固定为 1.18.0 系列），
// 仅做安全补丁更新，因此通常只有 1~3 个版本。想获取更多版本需添加 nginx.org 官方源。
func aptVersions() []string {
	out, err := runCmd("apt-cache", "madison", "nginx")
	if err != nil {
		return nil
	}
	return parseAptMadison(out)
}

// parseDnfList 解析 dnf/yum --showduplicates list 的输出，行格式：nginx.x86_64 版本 仓库
func parseDnfList(out string) []string {
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "nginx.") {
			versions = append(versions, fields[1])
		}
	}
	return dedupe(versions)
}

func dnfVersions(tool string) []string {
	out, err := runCmd(tool, "--showduplicates", "list", "nginx")
	if err != nil {
		return nil
	}
	return parseDnfList(out)
}

// parseApkPolicy 解析 apk policy 的输出，版本行形如恰好两个空格缩进的 "1.24.0-r6:"
func parseApkPolicy(out string) []string {
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(t, ":") {
			versions = append(versions, strings.TrimSuffix(t, ":"))
		}
	}
	return dedupe(versions)
}

func apkVersions() []string {
	out, err := runCmd("apk", "policy", "nginx")
	if err != nil {
		return nil
	}
	return parseApkPolicy(out)
}

// availableVersions 检测系统包管理器并查询可安装的 nginx 版本
func availableVersions() nginxAvailable {
	switch {
	case hasCmd("apt-cache"):
		return nginxAvailable{Supported: true, Manager: "apt", Versions: aptVersions()}
	case hasCmd("dnf"):
		return nginxAvailable{Supported: true, Manager: "dnf", Versions: dnfVersions("dnf")}
	case hasCmd("yum"):
		return nginxAvailable{Supported: true, Manager: "yum", Versions: dnfVersions("yum")}
	case hasCmd("apk"):
		return nginxAvailable{Supported: true, Manager: "apk", Versions: apkVersions()}
	}
	return nginxAvailable{Supported: true, Versions: []string{}}
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func nginxAvailableHandler(w http.ResponseWriter, r *http.Request) {
	if !isLinux() {
		writeJSON(w, nginxAvailable{Versions: []string{}})
		return
	}
	info := availableVersions()
	if info.Versions == nil {
		info.Versions = []string{}
	}
	writeJSON(w, info)
}

// ---------------------------------------------------------------- 安装任务
// 源码编译安装 nginx 通常要数分钟，同步等待会超时，因此做成异步任务：
// POST /api/nginx/install 立即返回 taskId，前端轮询 install/status 取进度与日志。

const configDirName = "config.d" // 站点配置目录，位于 conf 下

// 安装相关路径。定义为变量而非常量，是为了在单元测试中临时重定向到临时目录，
// 验证卸载逻辑确实只删除了目标路径、未波及无关文件（破坏性操作必须严格限定范围）。
var (
	installPrefix = "/usr/local/nginx" // 编译安装的目标目录
	sourceDir     = "/data/soft"       // 源码下载与解压目录
)

// nginx 编译时默认开启 -Werror（警告即错误）。OpenSSL 3.0 起（Ubuntu 22.04 默认
// 即为 3.0.2）把 HMAC_Init_ex、ENGINE_by_id、ENGINE_set_default 等接口标记为废弃，
// 而 nginx 1.24.0 及更早版本仍在调用，于是 deprecation 警告被 -Werror 升级为
// 编译错误，导致 ngx_event_openssl.c 编译中断。
// 这里只把这一类警告降级为普通告警（日志中依然可见），不影响其他警告的行为。
// 参考 nginx 官方 ticket #1964 中给出的处理方式。
const ccOpt = "-Wno-error=deprecated-declarations"

// 任务动作类型：安装与卸载共用同一套异步任务框架
const (
	actionInstall   = "install"
	actionUninstall = "uninstall"
)

// 只放行 x.y.z 形式的版本号。版本号最终会拼进下载 URL 与命令参数，
// 严格收敛格式可杜绝命令注入（如 "1.24.0; rm -rf /"）。
var strictVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// 匹配 "1.24.0" 或带 epoch 的 "1:1.24.0"，用于从各种版本串中提取主版本
var coreVersionRe = regexp.MustCompile(`\d+:\d+(?:\.\d+)+|\d+(?:\.\d+)+`)

// coreVersion 从 "1.24.0-2ubuntu7.1" / "nginx/1.24.0" / "1:1.24.0-1" 中提取 "1.24.0"
func coreVersion(raw string) string {
	m := coreVersionRe.FindString(strings.TrimSpace(raw))
	if i := strings.Index(m, ":"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

type installTask struct {
	mu         sync.Mutex
	ID         string
	Action     string // install / uninstall
	Version    string
	Status     string // running / success / failed
	Error      string
	Logs       []string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// installTaskView 是对外输出的快照，避免把带 mutex 的结构体按值拷贝
type installTaskView struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"`
	Version    string     `json:"version"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	Logs       []string   `json:"logs"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

var (
	taskMu sync.Mutex
	tasks  = make(map[string]*installTask)
)

func newTaskID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func (t *installTask) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	t.mu.Lock()
	t.Logs = append(t.Logs, line)
	// 编译输出量很大，保留最近若干条，避免内存无限增长
	if len(t.Logs) > 5000 {
		t.Logs = append([]string{}, t.Logs[len(t.Logs)-5000:]...)
	}
	t.mu.Unlock()
	log.Println("[install]", line)
}

func (t *installTask) setStatus(status, errMsg string) {
	t.mu.Lock()
	t.Status = status
	t.Error = errMsg
	t.mu.Unlock()
}

func (t *installTask) snapshot() installTaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	logs := make([]string, len(t.Logs))
	copy(logs, t.Logs)
	return installTaskView{
		ID:         t.ID,
		Action:     t.Action,
		Version:    t.Version,
		Status:     t.Status,
		Error:      t.Error,
		Logs:       logs,
		StartedAt:  t.StartedAt,
		FinishedAt: t.FinishedAt,
	}
}

// exec 在指定目录执行命令，把 stdout/stderr 逐行写入任务日志
func (t *installTask) exec(dir, name string, args ...string) error {
	t.logf("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for scanner.Scan() {
			t.logf("%s", scanner.Text())
		}
	}
	go pump(stdout)
	go pump(stderr)
	wg.Wait()

	return cmd.Wait()
}

// configureArgs 返回编译 nginx 的 configure 参数
func configureArgs() []string {
	return []string{
		"--prefix=" + installPrefix,
		"--with-http_ssl_module",
		"--with-http_v2_module",
		"--with-http_gzip_static_module",
		"--with-cc-opt=" + ccOpt,
	}
}

// ensureConfigInclude 在 nginx.conf 的 http 块末尾加入 config.d 引用。
// 用程序化改写替代交互式 vim 编辑；幂等，已包含则不重复添加。
func ensureConfigInclude(confPath string) error {
	confDir := filepath.Join(filepath.Dir(confPath), configDirName)
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", confDir, err)
	}
	data, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", confPath, err)
	}
	content := string(data)
	if strings.Contains(content, configDirName) {
		return nil
	}
	// nginx.conf 中最后一个 '}' 即 http 块的结束符，在它之前插入 include
	idx := strings.LastIndex(content, "}")
	if idx < 0 {
		return errors.New("未在 nginx.conf 中找到 http 块结束符")
	}
	updated := content[:idx] + "    include " + configDirName + "/*.conf;\n" + content[idx:]
	if err := os.WriteFile(confPath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", confPath, err)
	}
	return nil
}

// run 是异步任务的统一入口：根据 Action 分发到安装或卸载流程。
func (t *installTask) run() {
	defer func() {
		now := time.Now()
		t.mu.Lock()
		if t.Status == "running" {
			t.Status = "success"
		}
		if t.FinishedAt == nil {
			t.FinishedAt = &now
		}
		t.mu.Unlock()
	}()

	t.setStatus("running", "")

	if t.Action == actionUninstall {
		t.runUninstall()
		return
	}
	t.runInstall()
}

// runInstall 依次执行编译安装各步骤，任一步失败即终止并记录原因
func (t *installTask) runInstall() {
	pkgName := "nginx-" + t.Version
	tarball := pkgName + ".tar.gz"
	downloadURL := "https://nginx.org/download/" + tarball
	srcPath := filepath.Join(sourceDir, pkgName)

	if os.Geteuid() != 0 {
		t.logf("[warn] 当前进程 uid=%d，非 root 运行时下列步骤可能因权限不足而失败", os.Geteuid())
	}

	steps := []struct {
		desc string
		fn   func() error
	}{
		{"准备源码目录 " + sourceDir, func() error {
			return os.MkdirAll(sourceDir, 0o755)
		}},
		{"安装编译依赖", func() error {
			return t.exec("", "apt-get", "install", "-y",
				"build-essential", "libpcre3-dev", "zlib1g-dev", "libssl-dev")
		}},
		{"下载源码 " + downloadURL, func() error {
			if hasCmd("wget") {
				return t.exec(sourceDir, "wget", "-q", "--show-progress", downloadURL)
			}
			if hasCmd("curl") {
				return t.exec(sourceDir, "curl", "-fL", "-O", downloadURL)
			}
			return errors.New("未找到 wget 或 curl，无法下载源码")
		}},
		{"解压源码", func() error {
			// 重跑安装时，旧的 .o 与 Makefile 残留会导致再次失败，先清理同名目录
			if _, err := os.Stat(srcPath); err == nil {
				t.logf("发现已存在的源码目录，先清理: %s", srcPath)
				if err := os.RemoveAll(srcPath); err != nil {
					return fmt.Errorf("清理旧源码目录失败: %w", err)
				}
			}
			return t.exec(sourceDir, "tar", "xf", tarball)
		}},
		{"配置编译参数", func() error {
			return t.exec(srcPath, "./configure", configureArgs()...)
		}},
		{"编译源码（" + strconv.Itoa(runtime.NumCPU()) + " 核并行，耗时较长）", func() error {
			return t.exec(srcPath, "make", "-j"+strconv.Itoa(runtime.NumCPU()))
		}},
		{"安装到 " + installPrefix, func() error {
			return t.exec(srcPath, "make", "install")
		}},
		{"校验配置并启动服务", func() error {
			if err := t.exec("", installPrefix+"/sbin/nginx", "-t"); err != nil {
				return fmt.Errorf("配置校验失败: %w", err)
			}
			if err := t.exec("", installPrefix+"/sbin/nginx"); err != nil {
				return fmt.Errorf("启动失败（80 端口可能已被占用）: %w", err)
			}
			return t.exec("", "ln", "-sf", installPrefix+"/sbin/nginx", "/usr/bin/nginx")
		}},
		{"配置站点目录 config.d 并重载", func() error {
			if err := ensureConfigInclude(installPrefix + "/conf/nginx.conf"); err != nil {
				return err
			}
			return t.exec("", "/usr/bin/nginx", "-s", "reload")
		}},
	}

	for i, s := range steps {
		t.logf("==> [%d/%d] %s", i+1, len(steps), s.desc)
		if err := s.fn(); err != nil {
			t.logf("[错误] %v", err)
			t.setStatus("failed", s.desc+"失败: "+err.Error())
			return
		}
	}
	// 记录配置与执行路径。持久化失败不影响安装结果，仅作告警。
	if err := saveInstallRecord(newInstallRecord(t.Version, time.Now())); err != nil {
		t.logf("[warn] 安装信息记录失败（nginx 本身已安装成功）: %v", err)
	} else {
		t.logf("安装信息已写入 %s", recordFilePath())
	}
	t.logf("==> 全部步骤完成：nginx %s 已安装到 %s", t.Version, installPrefix)
}

// runUninstall 停止运行中的 nginx，并删除本工具安装的全部产物：
// 固定的安装前缀 /usr/local/nginx、全局软链 /usr/bin/nginx、源码目录，
// 以及位于安装前缀内的安装记录文件。
// 注意：本工具只管理自身编译安装到固定前缀的 nginx，不处理包管理器安装到
// /usr/sbin/nginx 的实例；卸载前请确认目标确为本工具所装。
func (t *installTask) runUninstall() {
	if os.Geteuid() != 0 {
		t.logf("[warn] 当前进程 uid=%d，非 root 运行时下列步骤可能因权限不足而失败", os.Geteuid())
	}

	steps := []struct {
		desc string
		fn   func() error
	}{
		{"停止运行中的 nginx", func() error {
			// 优先通过已安装的二进制优雅停止；未运行/无权限时静默忽略
			bin := installPrefix + "/sbin/nginx"
			if _, err := os.Stat(bin); err == nil {
				_ = t.exec("", bin, "-s", "stop")
			}
			// 兜底：结束所有 nginx 进程（含 master/worker），忽略报错
			_ = t.exec("", "pkill", "-x", "nginx")
			return nil
		}},
		{"删除全局软链 " + "/usr/bin/nginx", func() error {
			if err := os.Remove("/usr/bin/nginx"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("删除软链失败: %w", err)
			}
			return nil
		}},
		{"删除安装目录 " + installPrefix, func() error {
			if err := os.RemoveAll(installPrefix); err != nil {
				return fmt.Errorf("删除 %s 失败: %w", installPrefix, err)
			}
			return nil
		}},
		{"删除源码目录 " + filepath.Join(sourceDir, "nginx-"+t.Version), func() error {
			src := filepath.Join(sourceDir, "nginx-"+t.Version)
			if err := os.RemoveAll(src); err != nil {
				t.logf("[warn] 源码目录 %s 删除失败（可忽略）: %v", src, err)
			}
			return nil
		}},
	}

	for i, s := range steps {
		t.logf("==> [%d/%d] %s", i+1, len(steps), s.desc)
		if err := s.fn(); err != nil {
			t.logf("[错误] %v", err)
			t.setStatus("failed", s.desc+"失败: "+err.Error())
			return
		}
	}
	t.logf("==> 全部步骤完成：nginx %s 已卸载（安装目录、软链与源码目录均已清理）", t.Version)
}

func nginxInstallHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}

	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}

	version := coreVersion(req.Version)
	if !strictVersionRe.MatchString(version) {
		writeJSON(w, map[string]string{
			"error": "版本号格式非法，需为 x.y.z（解析结果: " + version + "）",
		})
		return
	}

	taskMu.Lock()
	for _, t := range tasks {
		t.mu.Lock()
		running := t.Status == "running"
		t.mu.Unlock()
		if running {
			taskMu.Unlock()
			writeJSON(w, map[string]string{"error": "已有安装任务正在进行，请等待其结束"})
			return
		}
	}
	task := &installTask{
		ID:        newTaskID(),
		Version:   version,
		Status:    "running",
		StartedAt: time.Now(),
	}
	tasks[task.ID] = task
	taskMu.Unlock()

	go task.run()

	writeJSON(w, map[string]string{"taskId": task.ID, "version": version})
}

// nginxUninstallHandler 启动卸载任务。卸载同样做成异步任务，复用状态轮询接口
// /api/nginx/install/status（任务框架与安装共用，快照中含 action 字段）。
func nginxUninstallHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}

	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}

	// 卸载作用于固定的安装前缀，版本仅用于定位源码目录，宽容处理（核心段即可）
	version := coreVersion(req.Version)

	taskMu.Lock()
	for _, t := range tasks {
		t.mu.Lock()
		running := t.Status == "running"
		t.mu.Unlock()
		if running {
			taskMu.Unlock()
			writeJSON(w, map[string]string{"error": "已有任务正在进行，请等待其结束"})
			return
		}
	}
	task := &installTask{
		ID:        newTaskID(),
		Action:    actionUninstall,
		Version:   version,
		Status:    "running",
		StartedAt: time.Now(),
	}
	tasks[task.ID] = task
	taskMu.Unlock()

	go task.run()

	writeJSON(w, map[string]string{"taskId": task.ID, "version": version})
}

func nginxInstallStatusHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("taskId")
	if id == "" {
		writeJSON(w, map[string]string{"error": "缺少 taskId"})
		return
	}
	taskMu.Lock()
	task, ok := tasks[id]
	taskMu.Unlock()
	if !ok {
		writeJSON(w, map[string]string{"error": "任务不存在"})
		return
	}
	writeJSON(w, task.snapshot())
}

// ---------------------------------------------------------------- 安装记录
// 编译安装会把文件分散到多个目录，事后很难回忆。这里在安装成功后把配置文件、
// 执行目录等关键路径持久化到 JSON，页面上可直接查看。

// 记录文件与 nginx 装在同一处，随安装目录一起存在
const recordFileName = "nginx-web-install.json"

type installRecord struct {
	Version       string    `json:"version"`
	InstalledAt   time.Time `json:"installedAt"`
	Prefix        string    `json:"prefix"`
	ConfigPath    string    `json:"configPath"`    // 主配置文件 nginx.conf
	ConfigDir     string    `json:"configDir"`     // 站点配置目录 config.d
	BinPath       string    `json:"binPath"`       // 编译产出的可执行文件
	SymlinkPath   string    `json:"symlinkPath"`   // 全局软链，可直接 nginx -v
	LogDir        string    `json:"logDir"`        // 访问与错误日志目录
	PidPath       string    `json:"pidPath"`       // 主进程 PID 文件
	SourceDir     string    `json:"sourceDir"`     // 解压后的源码目录
	ConfigureArgs []string  `json:"configureArgs"` // 当时的编译参数，便于复现
}

func newInstallRecord(version string, installedAt time.Time) installRecord {
	return installRecord{
		Version:       version,
		InstalledAt:   installedAt,
		Prefix:        installPrefix,
		ConfigPath:    filepath.Join(installPrefix, "conf", "nginx.conf"),
		ConfigDir:     filepath.Join(installPrefix, "conf", configDirName),
		BinPath:       filepath.Join(installPrefix, "sbin", "nginx"),
		SymlinkPath:   "/usr/bin/nginx",
		LogDir:        filepath.Join(installPrefix, "logs"),
		PidPath:       filepath.Join(installPrefix, "logs", "nginx.pid"),
		SourceDir:     filepath.Join(sourceDir, "nginx-"+version),
		ConfigureArgs: configureArgs(),
	}
}

func recordFilePath() string {
	return filepath.Join(installPrefix, recordFileName)
}

// loadRecordsFrom 从指定路径读取记录。文件不存在时返回空列表且不报错。
func loadRecordsFrom(path string) ([]installRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var records []installRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func loadInstallRecords() ([]installRecord, error) {
	return loadRecordsFrom(recordFilePath())
}

// saveRecordTo 把记录写入指定路径：同版本覆盖，不同版本追加，按时间倒序保存
func saveRecordTo(path string, rec installRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建记录目录失败: %w", err)
	}
	records, err := loadRecordsFrom(path)
	if err != nil {
		// 记录文件损坏不应影响安装结果，从空列表重建
		log.Printf("安装记录读取失败，将重建: %v", err)
		records = nil
	}
	replaced := false
	for i := range records {
		if records[i].Version == rec.Version {
			records[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].InstalledAt.After(records[j].InstalledAt)
	})
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化记录失败: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入记录失败: %w", err)
	}
	return nil
}

func saveInstallRecord(rec installRecord) error {
	return saveRecordTo(recordFilePath(), rec)
}

func nginxInstallsHandler(w http.ResponseWriter, r *http.Request) {
	if !isLinux() {
		writeJSON(w, map[string]any{"supported": false, "recordPath": "", "records": []installRecord{}})
		return
	}
	records, err := loadInstallRecords()
	if err != nil {
		log.Printf("读取安装记录失败: %v", err)
		records = nil
	}
	if records == nil {
		records = []installRecord{}
	}
	writeJSON(w, map[string]any{
		"supported":  true,
		"recordPath": recordFilePath(),
		"records":    records,
	})
}

// ---------------------------------------------------------------- 站点配置
// config.d 下的每个 .conf 文件即一个站点（nginx.conf 中已 include config.d/*.conf）。
// 本模块提供站点列表、新增、修改、删除与重载能力，覆盖「域名的新增、修改、重启」需求。

// configDir 返回站点配置目录（位于安装前缀的 conf 下）
func configDir() string {
	return filepath.Join(installPrefix, "conf", configDirName)
}

// nginxBin 优先使用安装时创建的全局软链，回退到安装前缀下的二进制
func nginxBin() string {
	if _, err := os.Stat("/usr/bin/nginx"); err == nil {
		return "/usr/bin/nginx"
	}
	return filepath.Join(installPrefix, "sbin", "nginx")
}

// siteConfig 描述一个站点的关键字段（由 .conf 文件解析而来）
type siteConfig struct {
	File        string `json:"file"`        // 配置文件名，作为唯一标识
	Domain      string `json:"domain"`      // server_name 首项；可为域名、IPv4/IPv6 地址或 _ 表示默认
	Listen      int    `json:"listen"`      // 监听端口
	Root        string `json:"root"`        // 网站根目录（静态托管模式）
	ProxyScheme string `json:"proxyScheme"` // 反代协议：http / https
	ProxyHost   string `json:"proxyHost"`   // 反代上游地址；空表示 127.0.0.1
	ProxyPort   int    `json:"proxyPort"`   // 反代上游端口；0 表示未启用（静态托管）
	SSL         bool   `json:"ssl"`         // 是否启用 HTTPS
	Cert        string `json:"cert"`        // 证书路径（ssl_certificate）；SSL 开启时必填
	Key         string `json:"key"`         // 私钥路径（ssl_certificate_key）；SSL 开启时必填
}

// siteNameRe 合法域名/通配符字符：字母、数字、点、连字符、下划线、通配符 *
var siteNameRe = regexp.MustCompile(`^[A-Za-z0-9.*_-]+$`)

// ipv4Re 仅含数字与点（疑似 IPv4 时再做强校验）
var ipv4Re = regexp.MustCompile(`^[\d.]+$`)

// ipv6Re IPv6 字符白名单（交由 nginx -t 做最终语义校验）
var ipv6Re = regexp.MustCompile(`^[A-Za-z0-9:]+$`)

// isValidIPv4 对形如 1.2.3.4 的地址做强校验：四段且每段 0~255
func isValidIPv4(name string) bool {
	parts := strings.Split(name, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

// isValidServerName 判定 server_name 是否合法：支持域名、通配符（*.example.com）、
// 默认占位 _，以及 IPv4 / IPv6 地址。新增站点不再限定为域名，也可用 IP 标识。
func isValidServerName(name string) bool {
	if name == "" {
		return false
	}
	if name == "_" {
		return true
	}
	// 疑似 IPv4（仅数字与点）：做强校验，避免 999.999.999.999 之类误通过
	if ipv4Re.MatchString(name) {
		return isValidIPv4(name)
	}
	// 疑似 IPv6（含冒号）：字符白名单即可，语义由 nginx -t 把关
	if strings.Contains(name, ":") {
		return ipv6Re.MatchString(name)
	}
	// 其余按域名/通配符处理
	return siteNameRe.MatchString(name)
}

// defaultProxyHost 反代上游留空时的默认值（本机回环）
const defaultProxyHost = "127.0.0.1"

// proxyHostRe 反代上游地址字符集：域名、IPv4、IPv6（含方括号写法）
var proxyHostRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]+$`)

// isValidProxyHost 判定反代上游地址是否合法。允许域名、IPv4、IPv6，
// 留空表示回环地址 127.0.0.1；路径分隔符与空白一律拒绝，防止注入 nginx 指令。
func isValidProxyHost(host string) bool {
	if host == "" {
		return true
	}
	if !proxyHostRe.MatchString(host) {
		return false
	}
	// 疑似 IPv4：复用强校验，挡掉 999.999.999.999
	if ipv4Re.MatchString(host) {
		return isValidIPv4(host)
	}
	// IPv6：去掉方括号后按字符白名单判定
	if strings.Contains(host, ":") {
		bare := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		return ipv6Re.MatchString(bare)
	}
	return true
}

// isValidProxyScheme 反代协议仅允许 http / https，留空按 http 处理
func isValidProxyScheme(scheme string) bool {
	switch scheme {
	case "", "http", "https":
		return true
	default:
		return false
	}
}

// normalizeProxyHost 渲染前规整上游地址：IPv6 裸地址需补方括号，否则 nginx 无法解析
func normalizeProxyHost(host string) string {
	if host == "" {
		return defaultProxyHost
	}
	// 已带方括号的不动；含两个及以上冒号判定为 IPv6
	if !strings.HasPrefix(host, "[") && strings.Count(host, ":") >= 2 {
		return "[" + host + "]"
	}
	return host
}

// splitProxyTarget 把 proxy_pass 的目标拆成主机与端口，如
// "127.0.0.1:3000" -> ("127.0.0.1", "3000")，"[::1]:8080" -> ("::1", "8080")。
// 主机部分本身含冒号（未加方括号的 IPv6）时不拆分，避免把地址尾段误当端口。
func splitProxyTarget(target string) (host, port string) {
	t := strings.TrimSpace(target)
	if strings.HasPrefix(t, "[") {
		if i := strings.Index(t, "]"); i >= 0 {
			host = t[1:i]
			if rest := strings.TrimPrefix(t[i+1:], ":"); rest != "" {
				port = rest
			}
			return host, port
		}
	}
	if i := strings.LastIndex(t, ":"); i > 0 && !strings.Contains(t[:i], ":") {
		return t[:i], t[i+1:]
	}
	return t, ""
}

// sanitizeSiteFile 把域名转成安全的文件名，杜绝路径穿越
func sanitizeSiteFile(domain string) string {
	name := strings.TrimSpace(domain)
	name = strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." {
		name = "site"
	}
	if !strings.HasSuffix(name, ".conf") {
		name += ".conf"
	}
	return name
}

// parseSiteConf 从 server 块文本中解析站点关键字段（File 字段不在此处设置）
func parseSiteConf(content string) (sc siteConfig) {
	if m := regexp.MustCompile(`listen\s+(\d+)`).FindStringSubmatch(content); m != nil {
		sc.Listen, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`server_name\s+([^;]+);`).FindStringSubmatch(content); m != nil {
		if fields := strings.Fields(m[1]); len(fields) > 0 {
			sc.Domain = fields[0]
		}
	}
	if m := regexp.MustCompile(`root\s+([^;]+);`).FindStringSubmatch(content); m != nil {
		sc.Root = strings.TrimSpace(m[1])
	}
	// SSL：listen ... ssl; 或存在 ssl_certificate 指令
	if regexp.MustCompile(`listen\s+\d+\s+ssl\s*;`).MatchString(content) ||
		regexp.MustCompile(`ssl_certificate\s+`).MatchString(content) {
		sc.SSL = true
	}
	// 反向代理目标：proxy_pass <scheme>://<host>:<port>，host 可能带 IPv6 方括号
	if m := regexp.MustCompile(`proxy_pass\s+(https?)://([^/\s]+):(\d+)\s*;`).FindStringSubmatch(content); m != nil {
		sc.ProxyScheme = m[1]
		// 去掉 IPv6 地址的方括号，存裸地址；渲染时再由 normalizeProxyHost 补回
		sc.ProxyHost = strings.TrimSuffix(strings.TrimPrefix(m[2], "["), "]")
		sc.ProxyPort, _ = strconv.Atoi(m[3])
	}
	if m := regexp.MustCompile(`ssl_certificate\s+([^;]+);`).FindStringSubmatch(content); m != nil {
		sc.Cert = strings.TrimSpace(m[1])
	}
	if m := regexp.MustCompile(`ssl_certificate_key\s+([^;]+);`).FindStringSubmatch(content); m != nil {
		sc.Key = strings.TrimSpace(m[1])
	}
	return sc
}

// renderSiteConf 根据站点输入生成 server 块文本内容。
// 反向代理模式（ProxyPort>0）生成 proxy_pass；否则静态托管生成 root + try_files。
// 启用 SSL 时 listen 带 ssl 参数并写入证书指令。
func renderSiteConf(in siteInput) string {
	listen := in.Listen
	if listen <= 0 {
		listen = 80
	}
	logName := strings.NewReplacer("/", "_", ":", "_").Replace(in.Domain)

	var b strings.Builder
	b.WriteString("server {\n")
	if in.SSL {
		fmt.Fprintf(&b, "    listen %d ssl;\n", listen)
	} else {
		fmt.Fprintf(&b, "    listen %d;\n", listen)
	}
	fmt.Fprintf(&b, "    server_name %s;\n", in.Domain)

	if in.SSL {
		b.WriteString("\n")
		fmt.Fprintf(&b, "    ssl_certificate %s;\n", in.Cert)
		fmt.Fprintf(&b, "    ssl_certificate_key %s;\n", in.Key)
	}

	if in.ProxyPort > 0 {
		b.WriteString("\n")
		b.WriteString("    location / {\n")
		// 反代上游地址可自定义（默认本机回环 127.0.0.1）；协议留空按 http 处理
		proxyScheme := in.ProxyScheme
		if proxyScheme == "" {
			proxyScheme = "http"
		}
		proxyTarget := normalizeProxyHost(in.ProxyHost)
		fmt.Fprintf(&b, "        proxy_pass %s://%s:%d;\n", proxyScheme, proxyTarget, in.ProxyPort)
		b.WriteString("        proxy_set_header Host $host;\n")
		b.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
		b.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
		b.WriteString("    }\n")
	} else {
		b.WriteString("\n")
		fmt.Fprintf(&b, "    root %s;\n", in.Root)
		b.WriteString("    index index.html index.htm;\n")
		b.WriteString("\n")
		b.WriteString("    location / {\n")
		b.WriteString("        try_files $uri $uri/ =404;\n")
		b.WriteString("    }\n")
	}

	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "    access_log %s/logs/%s.access.log;\n", installPrefix, logName)
	fmt.Fprintf(&b, "    error_log  %s/logs/%s.error.log;\n", installPrefix, logName)
	b.WriteString("}\n")
	return b.String()
}

// listSites 读取 config.d 下所有 .conf 文件并解析
func listSites() ([]siteConfig, error) {
	dir := configDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sites []siteConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := parseSiteConf(string(data))
		sc.File = e.Name()
		sites = append(sites, sc)
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].File < sites[j].File
	})
	return sites, nil
}

// sslBaseDir 证书存放根目录，位于 nginx 的 conf 下，随安装目录一起存在
func sslBaseDir() string {
	return filepath.Join(installPrefix, "conf", "ssl")
}

// certDirName 把站点标识转成安全的目录名，杜绝路径穿越。
// 先转义分隔符再取 Base：这样分隔符不会残留，随后去掉前导点，避免出现 ".._x" 这类怪名。
func certDirName(domain string) string {
	name := strings.TrimSpace(domain)
	name = strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(name)
	name = filepath.Base(name)
	name = strings.TrimLeft(name, ".") // 域名不会以点开头，去之可得更干净的名字
	if name == "" {
		name = "site"
	}
	return name
}

// certDirFor 返回该站点专属的证书目录，如 /usr/local/nginx/conf/ssl/example.com
func certDirFor(domain string) string {
	return filepath.Join(sslBaseDir(), certDirName(domain))
}

// safeJoin 把归档内的相对路径拼到 destDir 下，并拒绝逃出 destDir 的条目（zip-slip 防护）。
// 判定先按斜杠做，不依赖 filepath.IsAbs，以免不同平台语义差异导致漏判。
func safeJoin(destDir, name string) (string, error) {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return "", fmt.Errorf("归档内含有空路径条目")
	}
	slash := strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(slash, "/") {
		return "", fmt.Errorf("归档内含有绝对路径: %s", name)
	}
	if len(slash) >= 2 && slash[1] == ':' {
		return "", fmt.Errorf("归档内含有盘符路径: %s", name)
	}
	clean := filepath.Clean(filepath.FromSlash(slash))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("归档内含有越界路径: %s", name)
	}
	target := filepath.Join(destDir, clean)
	// 双重校验：拼接后的绝对路径必须仍在 destDir 之内
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if absTarget != absDest && !strings.HasPrefix(absTarget, absDest+string(filepath.Separator)) {
		return "", fmt.Errorf("归档内含有越界路径: %s", name)
	}
	return target, nil
}

// extractCertArchive 按文件后缀把证书包解压到 destDir。
// 支持 .zip / .tar.gz / .tgz / .tar；其余按单文件直接写入。
func extractCertArchive(destDir, filename string, data []byte) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("创建证书目录失败: %w", err)
	}
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(destDir, data)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(destDir, data)
	case strings.HasSuffix(lower, ".tar"):
		return extractTar(destDir, bytes.NewReader(data))
	default:
		// 单文件（.pem / .crt / .cer / .key 等）直接落盘
		target, err := safeJoin(destDir, filepath.Base(filepath.FromSlash(filename)))
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}
}

func extractZip(destDir string, data []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("zip 解析失败: %w", err)
	}
	for _, f := range zr.File {
		target, err := safeJoin(destDir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeFromReader(target, func() (io.ReadCloser, error) { return f.Open() }, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(destDir string, data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip 解析失败: %w", err)
	}
	defer gz.Close()
	return extractTar(destDir, gz)
}

func extractTar(destDir string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar 解析失败: %w", err)
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFromReader(target, func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		}
	}
}

// writeFromReader 打开源并落盘，统一走这个入口确保文件句柄被关闭
func writeFromReader(target string, open func() (io.ReadCloser, error), mode os.FileMode) error {
	src, err := open()
	if err != nil {
		return err
	}
	defer src.Close()
	if mode == 0 {
		mode = 0o644
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Close()
}

// classifyPEM 依据内容判定文件类型：证书 / 私钥 / 未知。
// 只看内容不看扩展名，因为各家 CA 下发的文件名并不统一。
func classifyPEM(content []byte) string {
	s := string(content)
	switch {
	case strings.Contains(s, "BEGIN CERTIFICATE"):
		return "cert"
	case strings.Contains(s, "PRIVATE KEY"):
		// 覆盖 BEGIN PRIVATE KEY / BEGIN RSA PRIVATE KEY / BEGIN EC PRIVATE KEY 等
		return "key"
	default:
		return ""
	}
}

// certExts / keyExts 内容无法判定时（如空的占位文件）的兜底依据
var certExts = map[string]bool{".pem": true, ".crt": true, ".cer": true}
var keyExts = map[string]bool{".key": true}

// pickCertAndKey 在证书目录中递归查找证书与私钥，返回其绝对路径。
// 优先按内容判定，其次按扩展名，同类型多个时取路径排序靠前者，保证结果稳定。
func pickCertAndKey(dir string) (certPath, keyPath string, err error) {
	var certs, keys []string
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// 私钥优先使用 0600 之外的常规读取；读取失败不影响其他文件
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch classifyPEM(data) {
		case "cert":
			certs = append(certs, path)
		case "key":
			keys = append(keys, path)
		default:
			if certExts[ext] {
				certs = append(certs, path)
			} else if keyExts[ext] {
				keys = append(keys, path)
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	sort.Strings(certs)
	sort.Strings(keys)
	if len(certs) > 0 {
		certPath = certs[0]
	}
	if len(keys) > 0 {
		keyPath = keys[0]
	}
	return certPath, keyPath, nil
}

// listCertFiles 列出证书目录下的相对路径清单，供前端展示解压结果
func listCertFiles(dir string) []string {
	var out []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = filepath.Base(path)
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

// nginxSitesHandler 返回站点列表与 nginx 安装状态
func nginxSitesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 GET"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]any{"supported": false, "sites": []siteConfig{}})
		return
	}
	nginxInstalled := false
	if _, err := os.Stat(filepath.Join(installPrefix, "sbin", "nginx")); err == nil {
		nginxInstalled = true
	}
	resp := map[string]any{
		"supported":      true,
		"nginxInstalled": nginxInstalled,
		"configDir":      configDir(),
	}
	if !nginxInstalled {
		resp["sites"] = []siteConfig{}
		writeJSON(w, resp)
		return
	}
	sites, err := listSites()
	if err != nil {
		// config.d 不存在（尚未初始化）时视为空列表
		if os.IsNotExist(err) {
			resp["sites"] = []siteConfig{}
		} else {
			resp["error"] = "读取站点配置失败: " + err.Error()
			resp["sites"] = []siteConfig{}
		}
		writeJSON(w, resp)
		return
	}
	resp["sites"] = sites
	writeJSON(w, resp)
}

// siteInput 是新增/修改请求体
type siteInput struct {
	File        string `json:"file"`        // 修改时必填，定位已有文件
	Domain      string `json:"domain"`      // server_name
	Listen      int    `json:"listen"`      // 监听端口
	Root        string `json:"root"`        // 网站根目录（静态托管模式）
	ProxyScheme string `json:"proxyScheme"` // 反代协议：http / https，空按 http
	ProxyHost   string `json:"proxyHost"`   // 反代上游地址，空按 127.0.0.1
	ProxyPort   int    `json:"proxyPort"`   // 反代上游端口；0 表示未启用
	SSL         bool   `json:"ssl"`         // 是否启用 HTTPS
	Cert        string `json:"cert"`        // 证书路径（ssl 开启时必填）
	Key         string `json:"key"`         // 私钥路径（ssl 开启时必填）
}

func (s siteInput) validate() string {
	if s.Domain == "" {
		return "站点标识（域名或 IP）不能为空"
	}
	if !isValidServerName(s.Domain) {
		return "站点标识非法：应为域名（如 example.com）、通配符（*.example.com）、默认占位 _，或 IP 地址（如 192.168.1.1、2001:db8::1）"
	}
	if s.Listen <= 0 || s.Listen > 65535 {
		return "监听端口需在 1~65535 之间"
	}
	if s.ProxyPort < 0 || s.ProxyPort > 65535 {
		return "代理端口需在 1~65535 之间（留空表示不启用反向代理）"
	}
	// 启用 HTTPS 时必须提供证书与私钥（绝对路径）
	if s.SSL {
		if s.Cert == "" || s.Key == "" {
			return "启用 HTTPS 时需填写证书与私钥路径"
		}
		if !strings.HasPrefix(s.Cert, "/") || !strings.HasPrefix(s.Key, "/") {
			return "证书与私钥路径需为绝对路径"
		}
	}
	// 反向代理模式：由 proxy_pass 转发，不必填根目录
	if s.ProxyPort > 0 {
		if !isValidProxyScheme(s.ProxyScheme) {
			return "反代协议非法，仅支持 http / https"
		}
		if !isValidProxyHost(s.ProxyHost) {
			return "反代上游地址非法（支持域名、IPv4、IPv6，留空表示本机 127.0.0.1）"
		}
		return ""
	}
	// 静态托管模式：必须填写根目录
	if s.Root == "" {
		return "网站根目录不能为空"
	}
	if !strings.HasPrefix(s.Root, "/") {
		return "网站根目录需为绝对路径"
	}
	return ""
}

// nginxSiteCreateHandler 新增站点配置
func nginxSiteCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}
	var in siteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, map[string]string{"error": msg})
		return
	}
	dir := configDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, map[string]string{"error": "创建配置目录失败: " + err.Error()})
		return
	}
	file := sanitizeSiteFile(in.Domain)
	path := filepath.Join(dir, file)
	// 防覆盖：新增不允许与已有文件同名
	if _, err := os.Stat(path); err == nil {
		writeJSON(w, map[string]string{"error": "同名站点已存在: " + file})
		return
	}
	content := renderSiteConf(in)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		writeJSON(w, map[string]string{"error": "写入配置文件失败: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "file": file})
}

// nginxSiteUpdateHandler 修改已有站点配置（按 file 定位；域名变更会换文件名）
func nginxSiteUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}
	var in siteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if in.File == "" {
		writeJSON(w, map[string]string{"error": "缺少 file 字段，无法定位待修改站点"})
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, map[string]string{"error": msg})
		return
	}
	dir := configDir()
	oldPath := filepath.Join(dir, filepath.Base(in.File))
	if _, err := os.Stat(oldPath); err != nil {
		writeJSON(w, map[string]string{"error": "待修改站点不存在: " + filepath.Base(in.File)})
		return
	}
	newFile := sanitizeSiteFile(in.Domain)
	newPath := filepath.Join(dir, newFile)
	content := renderSiteConf(in)
	// 先写新文件，成功后再删旧文件，避免中途失败丢失配置
	if err := os.WriteFile(newPath, []byte(content), 0o644); err != nil {
		writeJSON(w, map[string]string{"error": "写入配置文件失败: " + err.Error()})
		return
	}
	if newPath != oldPath {
		if err := os.Remove(oldPath); err != nil {
			writeJSON(w, map[string]string{"error": "更新成功但删除旧文件失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "file": newFile})
}

// nginxSiteDeleteHandler 删除站点配置
func nginxSiteDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}
	var in struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if in.File == "" {
		writeJSON(w, map[string]string{"error": "缺少 file 字段"})
		return
	}
	path := filepath.Join(configDir(), filepath.Base(in.File))
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, map[string]string{"error": "站点不存在: " + filepath.Base(in.File)})
			return
		}
		writeJSON(w, map[string]string{"error": "删除失败: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// maxCertUpload 证书包体积上限（32MB），远超实际所需，用于挡住误传的大文件
const maxCertUpload = 32 << 20

// nginxSiteCertUploadHandler 接收上传的证书包，按域名归置到 conf/ssl/<域名>/ 并自动解压。
// 返回识别出的证书与私钥绝对路径，供前端回填到表单（保存站点时写入 nginx 配置）。
func nginxSiteCertUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}
	if err := r.ParseMultipartForm(maxCertUpload); err != nil {
		writeJSON(w, map[string]string{"error": "上传内容解析失败: " + err.Error()})
		return
	}
	domain := strings.TrimSpace(r.FormValue("domain"))
	if domain == "" {
		writeJSON(w, map[string]string{"error": "请先填写域名 / IP，再上传证书"})
		return
	}
	if !isValidServerName(domain) {
		writeJSON(w, map[string]string{"error": "站点标识非法，无法作为证书目录名"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, map[string]string{"error": "读取上传文件失败: " + err.Error()})
		return
	}
	defer file.Close()

	if header.Size > maxCertUpload {
		writeJSON(w, map[string]string{"error": "证书包过大（上限 32MB）"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCertUpload+1))
	if err != nil {
		writeJSON(w, map[string]string{"error": "读取上传内容失败: " + err.Error()})
		return
	}
	if len(data) == 0 {
		writeJSON(w, map[string]string{"error": "上传文件为空"})
		return
	}

	// 同一域名重复上传时先清空旧目录，避免残留文件干扰识别
	destDir := certDirFor(domain)
	if err := os.RemoveAll(destDir); err != nil {
		writeJSON(w, map[string]string{"error": "清理旧证书目录失败: " + err.Error()})
		return
	}
	if err := extractCertArchive(destDir, header.Filename, data); err != nil {
		writeJSON(w, map[string]string{"error": "解压证书包失败: " + err.Error()})
		return
	}
	certPath, keyPath, err := pickCertAndKey(destDir)
	if err != nil {
		writeJSON(w, map[string]string{"error": "识别证书文件失败: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"ok":    true,
		"dir":   destDir,
		"cert":  certPath,
		"key":   keyPath,
		"files": listCertFiles(destDir),
	})
}

// nginxReloadHandler 重载（必要时重启）nginx，使站点配置变更生效。
// 先 nginx -t 校验配置，避免把错误配置应用上线；校验通过后尝试优雅重载，
// 若未运行则改为直接启动。
func nginxReloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeJSON(w, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !isLinux() {
		writeJSON(w, map[string]string{"error": "该功能仅支持 Linux（含 Ubuntu）系统"})
		return
	}
	bin := nginxBin()
	if _, err := os.Stat(bin); err != nil {
		writeJSON(w, map[string]string{"error": "未找到 nginx 可执行文件: " + bin})
		return
	}
	// 1) 配置校验（输出在 stderr，CombinedOutput 一并捕获）
	out, err := exec.Command(bin, "-t").CombinedOutput()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "output": string(out), "error": "配置校验失败: " + err.Error()})
		return
	}
	// 2) 尝试优雅重载；若未运行（无 PID）则启动
	if err := exec.Command(bin, "-s", "reload").Run(); err == nil {
		writeJSON(w, map[string]any{"ok": true, "output": string(out), "action": "reload"})
		return
	}
	// 3) 重载失败（多半是 nginx 未运行），直接启动
	if err := exec.Command(bin).Run(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "output": string(out), "error": "重启失败: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "output": string(out), "action": "start"})
}

// logging 记录每个请求的方法、路径与耗时，便于开发期排障
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// apiPort 读取 PORT 环境变量，非法值时回退 8080
func apiPort() string {
	p := os.Getenv("PORT")
	if p == "" {
		return "8080"
	}
	if n, err := strconv.Atoi(p); err != nil || n <= 0 || n > 65535 {
		log.Printf("警告: PORT=%q 不是合法端口，回退到 8080", p)
		return "8080"
	}
	return p
}

// registerWebroot 在构建时通过 -tags embed 把前端构建产物（web/dist）编译进二进制，
// 这里注册一个兜底路由：除 /api 之外的未知路径全部回退到 index.html（SPA 单页应用）。
// 不带 embed 标签构建时（开发模式、常规单测）embedWebroot 为 false，不注册该路由，
// 前端仍由 Vite 单独提供，后端只暴露 /api，行为与之前完全一致。
func registerWebroot(mux *http.ServeMux) {
	if !embedWebroot {
		return
	}
	sub, err := fs.Sub(webrootFS, "dist")
	if err != nil {
		log.Printf("[warn] 嵌入前端资源加载失败，静态页面不可用: %v", err)
		return
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /api* 一律交给既有 handler；这里只兜底非 API 的页面 / 静态资源请求
		if strings.HasPrefix(r.URL.Path, "/api") {
			http.NotFound(w, r)
			return
		}
		upath := strings.TrimPrefix(r.URL.Path, "/")
		if upath == "" {
			upath = "index.html"
		}
		// 资源存在则按正常流程返回（含正确的 Content-Type）
		if _, statErr := fs.Stat(sub, upath); statErr == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA 路由回退：未知路径直接返回 index.html 内容。
		// 注意：不能用把 r.URL.Path 改写为 /index.html 再交给 http.FileServer
		// 的方式——FileServer 会把 /index.html 301 重定向到 /，导致不跟随
		// 重定向的客户端（部分 curl / 程序化调用）拿到空响应。
		data, readErr := fs.ReadFile(sub, "index.html")
		if readErr != nil {
			http.Error(w, "index.html not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
}

func main() {
	// 读取登录密码（.env / 环境变量 / 默认 admin），在任何接口处理前完成
	initPassword()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", helloHandler)
	mux.HandleFunc("/api/login", loginHandler)
	mux.HandleFunc("/api/me", meHandler)
	mux.HandleFunc("/api/logout", logoutHandler)
	mux.HandleFunc("/api/nginx", nginxHandler)
	mux.HandleFunc("/api/nginx/available", nginxAvailableHandler)
	mux.HandleFunc("/api/nginx/install", nginxInstallHandler)
	mux.HandleFunc("/api/nginx/install/status", nginxInstallStatusHandler)
	mux.HandleFunc("/api/nginx/uninstall", nginxUninstallHandler)
	mux.HandleFunc("/api/nginx/installs", nginxInstallsHandler)
	mux.HandleFunc("/api/nginx/sites", nginxSitesHandler)
	mux.HandleFunc("/api/nginx/sites/create", nginxSiteCreateHandler)
	mux.HandleFunc("/api/nginx/sites/update", nginxSiteUpdateHandler)
	mux.HandleFunc("/api/nginx/sites/delete", nginxSiteDeleteHandler)
	mux.HandleFunc("/api/nginx/sites/cert", nginxSiteCertUploadHandler)
	mux.HandleFunc("/api/nginx/reload", nginxReloadHandler)

	// 单文件构建（-tags embed）时，注册静态页面兜底路由；否则无操作
	registerWebroot(mux)

	addr := ":" + apiPort()
	srv := &http.Server{
		Addr:              addr,
		Handler:           logging(requireAuth(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Println("server listening on", addr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("监听 %s 失败: %v", addr, err)
			log.Printf("提示: 端口可能被占用。WSL2 下 Windows 侧占用的端口同样会导致绑定失败，可尝试 PORT=8090 换端口启动")
			os.Exit(1)
		}
	}()

	// 优雅关闭：收到 SIGTERM/SIGINT 后停止接收新请求并等待处理完成
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("收到信号 %v，开始优雅关闭", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("优雅关闭超时，强制退出: %v", err)
	}
	log.Println("server stopped")
}
