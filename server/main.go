package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	File   string `json:"file"`   // 配置文件名，作为唯一标识
	Domain string `json:"domain"` // server_name 首项；可为域名、IPv4/IPv6 地址或 _ 表示默认
	Listen int    `json:"listen"` // 监听端口
	Root   string `json:"root"`   // 网站根目录
}

// siteNameRe 合法域名/通配符字符：字母、数字、点、连字符、下划线、通配符 *
var siteNameRe = regexp.MustCompile(`^[A-Za-z0-9.*_-]+$`)

// ipv4Re 仅含数字与点（疑似 IPv4 时再做强校验）
var ipv4Re = regexp.MustCompile(`^[\d.]+$`)

// ipv6Re IPv6 字符白名单（交由 nginx -t 做最终语义校验）
var ipv6Re = regexp.MustCompile(`^[A-Za-z0-9:]+$`)

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
	// 疑似 IPv6（含冒号）：字符白名单即可，语义由 nginx -t 把关
	if strings.Contains(name, ":") {
		return ipv6Re.MatchString(name)
	}
	// 其余按域名/通配符处理
	return siteNameRe.MatchString(name)
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

// parseSiteConf 从 server 块文本中提取域名、监听端口与根目录
func parseSiteConf(content string) (domain string, listen int, root string) {
	if m := regexp.MustCompile(`listen\s+(\d+)`).FindStringSubmatch(content); m != nil {
		listen, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`server_name\s+([^;]+);`).FindStringSubmatch(content); m != nil {
		if fields := strings.Fields(m[1]); len(fields) > 0 {
			domain = fields[0]
		}
	}
	if m := regexp.MustCompile(`root\s+([^;]+);`).FindStringSubmatch(content); m != nil {
		root = strings.TrimSpace(m[1])
	}
	return domain, listen, root
}

// renderSiteConf 生成 server 块文本内容
func renderSiteConf(domain string, listen int, root string) string {
	if listen <= 0 {
		listen = 80
	}
	logName := strings.NewReplacer("/", "_", ":", "_").Replace(domain)
	return fmt.Sprintf(`server {
    listen %d;
    server_name %s;

    root %s;
    index index.html index.htm;

    location / {
        try_files $uri $uri/ =404;
    }

    access_log %s/logs/%s.access.log;
    error_log  %s/logs/%s.error.log;
}
`, listen, domain, root, installPrefix, logName, installPrefix, logName)
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
		domain, listen, root := parseSiteConf(string(data))
		sites = append(sites, siteConfig{
			File:   e.Name(),
			Domain: domain,
			Listen: listen,
			Root:   root,
		})
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].File < sites[j].File
	})
	return sites, nil
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
	File   string `json:"file"` // 修改时必填，定位已有文件
	Domain string `json:"domain"`
	Listen int    `json:"listen"`
	Root   string `json:"root"`
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
	content := renderSiteConf(in.Domain, in.Listen, in.Root)
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
	content := renderSiteConf(in.Domain, in.Listen, in.Root)
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

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", helloHandler)
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
	mux.HandleFunc("/api/nginx/reload", nginxReloadHandler)

	addr := ":" + apiPort()
	srv := &http.Server{
		Addr:              addr,
		Handler:           logging(mux),
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
