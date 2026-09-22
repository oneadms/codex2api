package proxy

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"github.com/codex2api/auth"
	"golang.org/x/net/publicsuffix"
)

// codexTicketCookies 是一次上游交互后与门票绑定的 Cookie 快照，不进入用量日志。
type codexTicketCookies struct {
	Header     string
	ExpiresAt  time.Time
	CapturedAt time.Time
}

// captureCodexTicketCookies 按上游地址应用 Set-Cookie，保留请求中仍有效的 Cookie。
// CookieJar 负责域、路径、Secure 和删除语义，不能直接把 Set-Cookie 原文当请求头转发。
func captureCodexTicketCookies(endpoint *url.URL, previous codexTicketCookies, response *http.Response) codexTicketCookies {
	if endpoint == nil || response == nil {
		return codexTicketCookies{}
	}
	now := time.Now()
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	updates := response.Cookies()
	capturedAt := previous.CapturedAt
	// 缓存里只保留请求头，不含原始 Path。更新同名 Cookie 时先移除旧值，
	// 避免重新建 Jar 后因路径不同而同时发送新旧两个值。
	replaced := make(map[string]bool)
	for _, update := range updates {
		candidate := *update
		candidate.MaxAge = 0
		candidate.Expires = time.Time{}
		scope, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		scope.SetCookies(endpoint, []*http.Cookie{&candidate})
		if len(scope.Cookies(endpoint)) > 0 {
			replaced[update.Name] = true
		}
	}
	var expires time.Time
	if header := auth.NormalizeCodexTicketCookie(previous.Header); header != "" && (previous.ExpiresAt.IsZero() || now.Before(previous.ExpiresAt)) {
		cookies, _ := http.ParseCookie(header)
		for _, cookie := range cookies {
			if replaced[cookie.Name] {
				continue
			}
			cookie.Path = "/"
			jar.SetCookies(endpoint, []*http.Cookie{cookie})
			expires = previous.ExpiresAt
		}
	}
	jar.SetCookies(endpoint, updates)
	// 只有作用于目标地址的更新才重置新鲜期；无关域和无关路径不算更新。
	if len(replaced) > 0 {
		capturedAt = now
	}
	active := jar.Cookies(endpoint)
	if len(active) == 0 {
		return codexTicketCookies{}
	}
	request := &http.Request{Header: make(http.Header)}
	for _, cookie := range active {
		request.AddCookie(cookie)
		for _, update := range updates {
			if update.Name != cookie.Name || update.Value != cookie.Value {
				continue
			}
			deadline := update.Expires
			if update.MaxAge > 0 {
				deadline = now.Add(time.Duration(update.MaxAge) * time.Second)
			}
			if !deadline.IsZero() && (expires.IsZero() || deadline.Before(expires)) {
				expires = deadline
			}
		}
	}
	return codexTicketCookies{Header: auth.NormalizeCodexTicketCookie(request.Header.Get("Cookie")), ExpiresAt: expires, CapturedAt: capturedAt}
}
