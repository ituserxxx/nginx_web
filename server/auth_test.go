package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 登录鉴权测试

func TestParseDotEnv(t *testing.T) {
	m := parseDotEnv("# comment\nAPP_PASSWORD=secret\nFOO=\"bar baz\"\n\nEMPTY=\nKEY='v'\n")
	if m["APP_PASSWORD"] != "secret" {
		t.Errorf("APP_PASSWORD 解析错误: %q", m["APP_PASSWORD"])
	}
	if m["FOO"] != "bar baz" {
		t.Errorf("引号未去除: %q", m["FOO"])
	}
	if m["EMPTY"] != "" {
		t.Errorf("EMPTY 应为空: %q", m["EMPTY"])
	}
	if m["KEY"] != "v" {
		t.Errorf("单引号未去除: %q", m["KEY"])
	}
}

func TestLoginAndSession(t *testing.T) {
	appPassword = "admin"
	sessionMu.Lock()
	sessions = make(map[string]time.Time)
	sessionMu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", loginHandler)
	mux.HandleFunc("/api/me", meHandler)
	mux.HandleFunc("/api/logout", logoutHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 错误密码应被拒绝
	resp, _ := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"wrong"}`))
	var re map[string]string
	json.NewDecoder(resp.Body).Decode(&re)
	resp.Body.Close()
	if re["error"] == "" {
		t.Error("错误密码应当返回 error")
	}

	// 正确密码应下发 Cookie
	resp2, _ := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"admin"}`))
	var re2 map[string]string
	json.NewDecoder(resp2.Body).Decode(&re2)
	resp2.Body.Close()
	if re2["error"] != "" {
		t.Fatalf("正确密码应当成功: %v", re2)
	}
	var cookie string
	for _, c := range resp2.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("正确密码未下发会话 Cookie")
	}

	// 无 Cookie 访问 /api/me 应未登录
	resp3, _ := http.Get(srv.URL + "/api/me")
	var re3 map[string]bool
	json.NewDecoder(resp3.Body).Decode(&re3)
	resp3.Body.Close()
	if re3["authenticated"] {
		t.Error("无 Cookie 应为未登录")
	}

	// 带 Cookie 访问 /api/me 应为已登录
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Cookie", cookie)
	resp4, _ := http.DefaultClient.Do(req)
	var re4 map[string]bool
	json.NewDecoder(resp4.Body).Decode(&re4)
	resp4.Body.Close()
	if !re4["authenticated"] {
		t.Error("携带有效 Cookie 应为已登录")
	}

	// 登出后再查应为未登录
	reqL, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/logout", nil)
	reqL.Header.Set("Cookie", cookie)
	http.DefaultClient.Do(reqL)
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req2.Header.Set("Cookie", cookie)
	resp5, _ := http.DefaultClient.Do(req2)
	var re5 map[string]bool
	json.NewDecoder(resp5.Body).Decode(&re5)
	resp5.Body.Close()
	if re5["authenticated"] {
		t.Error("登出后应为未登录")
	}
}

func TestRequireAuth(t *testing.T) {
	appPassword = "admin"
	sessionMu.Lock()
	sessions = make(map[string]time.Time)
	sessionMu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", helloHandler)
	mux.HandleFunc("/api/login", loginHandler)
	mux.HandleFunc("/api/me", meHandler)
	mux.HandleFunc("/api/logout", logoutHandler)
	mux.HandleFunc("/api/nginx", nginxHandler)
	srv := httptest.NewServer(requireAuth(mux))
	defer srv.Close()

	// 公开接口无需登录
	r1, _ := http.Get(srv.URL + "/api/hello")
	if r1.StatusCode != http.StatusOK {
		t.Errorf("/api/hello 应为公开，实际 %d", r1.StatusCode)
	}
	r1.Body.Close()

	// 受保护接口未登录应 401
	r2, _ := http.Get(srv.URL + "/api/nginx")
	if r2.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/nginx 未登录应 401，实际 %d", r2.StatusCode)
	}
	r2.Body.Close()

	// 登录后受保护接口应放行
	resp, _ := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"admin"}`))
	resp.Body.Close()
	var ck string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			ck = c.Name + "=" + c.Value
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/nginx", nil)
	req.Header.Set("Cookie", ck)
	r3, _ := http.DefaultClient.Do(req)
	if r3.StatusCode != http.StatusOK {
		t.Errorf("/api/nginx 登录后应 200，实际 %d", r3.StatusCode)
	}
	r3.Body.Close()
}
