package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
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

// aptVersions 解析 apt-cache madison nginx，格式：nginx | 版本 | 源
func aptVersions() []string {
	out, err := runCmd("apt-cache", "madison", "nginx")
	if err != nil {
		return nil
	}
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "nginx" && fields[1] == "|" {
			versions = append(versions, fields[2])
		}
	}
	return dedupe(versions)
}

// dnfVersions 解析 dnf/yum --showduplicates list nginx，格式：nginx.x86_64 版本 仓库
func dnfVersions(tool string) []string {
	out, err := runCmd(tool, "--showduplicates", "list", "nginx")
	if err != nil {
		return nil
	}
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "nginx.") {
			versions = append(versions, fields[1])
		}
	}
	return dedupe(versions)
}

// apkVersions 解析 apk policy nginx，版本行形如两个空格缩进的 "1.24.0-r6:"
func apkVersions() []string {
	out, err := runCmd("apk", "policy", "nginx")
	if err != nil {
		return nil
	}
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(t, ":") {
			versions = append(versions, strings.TrimSuffix(t, ":"))
		}
	}
	return dedupe(versions)
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

func main() {
	http.HandleFunc("/api/hello", helloHandler)
	http.HandleFunc("/api/nginx", nginxHandler)
	http.HandleFunc("/api/nginx/available", nginxAvailableHandler)
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Println("server listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
