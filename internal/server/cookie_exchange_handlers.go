package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/auth"
)

// exchangeBodyLimit 是增量请求最大字节数，防止大头部占用内存。
const exchangeBodyLimit = 256 << 10

// exchangeVersionPattern 校验透传的固定版本格式，不接受任意字符串。
var exchangeVersionPattern = regexp.MustCompile(`^v1:[a-f0-9]{64}$`)

// exchangeCookieDTO 是唯一允许导出平台 Cookie 明文的授权 transport 模型，禁止复用于普通账号接口。
type exchangeCookieDTO struct {
	// Name 是平台 Cookie 名称。
	Name string `json:"name"`
	// Value 是授权交换的明文值，禁止记录。
	Value string `json:"value"`
	// Domain 保留域和主机作用域。
	Domain string `json:"domain"`
	// Path 保留路径作用域。
	Path string `json:"path"`
	// Expires 是 Unix 秒，非正表示会话。
	Expires float64 `json:"expires,omitempty"`
	// HTTPOnly 保留脚本不可读属性。
	HTTPOnly bool `json:"httpOnly,omitempty"`
	// Secure 保留 HTTPS 属性。
	Secure bool `json:"secure,omitempty"`
	// SameSite 保留跨站属性。
	SameSite string `json:"sameSite,omitempty"`
	// PartitionKey 是分区站点，空值表示不分区。
	PartitionKey string `json:"partitionKey,omitempty"`
}

// exchangeSnapshotDTO 只在明确会话和归属授权后输出。
type exchangeSnapshotDTO struct {
	// AccountID 绑定单个授权账号。
	AccountID string `json:"account_id"`
	// Version 是状态指纹，不能证明登录有效。
	Version string `json:"credential_version"`
	// Complete 成功时固定为 true。
	Complete bool `json:"snapshot_complete"`
	// Cookies 是短生命周期的明文快照。
	Cookies []exchangeCookieDTO `json:"cookies"`
}

// exchangeBatchDTO 保留响应作用域、接收时刻和逐条头部，不接受业务正文。
type exchangeBatchDTO struct {
	// URL 不包含签名查询和 fragment，仅用于解析 Cookie。
	URL string `json:"response_url"`
	// ReceivedAt 使用 UTC RFC3339 毫秒。
	ReceivedAt string `json:"received_at"`
	// SetCookies 是逐条响应头，禁止日志输出。
	SetCookies []string `json:"set_cookies"`
}

// exchangeUpdatesDTO 是增量提交的严格具名契约。
type exchangeUpdatesDTO struct {
	// Version 是调用者此前取得的版本。
	Version string `json:"credential_version"`
	// Responses 按接收顺序排列，不允许空增量。
	Responses []exchangeBatchDTO `json:"responses"`
}

// exchangeResultDTO 不包含任何秘密；failed 表示已保存但同步失败。
type exchangeResultDTO struct {
	// AccountID 是提交账号。
	AccountID string `json:"account_id"`
	// Changed 表示状态变化。
	Changed bool `json:"changed"`
	// Version 是提交后状态指纹。
	Version string `json:"credential_version"`
	// SyncStatus 是 synced、not_running、not_needed 或 failed。
	SyncStatus string `json:"runtime_sync_status"`
}

// mountCookieExchangeRoutes 在 r 中集中挂载两条接口，复用会话认证，不增加登录行为。
func (s *Server) mountCookieExchangeRoutes(r chi.Router) {
	// r 是交换接口专用路由组，认证失败前也禁止缓存响应。
	r.Group(func(r chi.Router) {
		r.Use(cookieExchangeNoStore)
		r.Use(s.Auth.Middleware)
		r.Use(auth.RequireAuth)
		r.Get("/api/v1/integrations/accounts/{account_id}/cookie-snapshot", s.cookieExchangeSnapshot)
		r.Post("/api/v1/integrations/accounts/{account_id}/cookie-updates", s.cookieExchangeUpdates)
	})
}

// cookieExchangeNoStore 为 next 的全部成功及失败响应禁止缓存，不捕获任何明文。
func cookieExchangeNoStore(next http.Handler) http.Handler {
	// w、r 是当前响应和请求；缓存头在认证中间件之前设置。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// cookieExchangeSnapshot 用 w、r 读取当前用户单个账号快照，不触发续期或平台请求。
func (s *Server) cookieExchangeSnapshot(w http.ResponseWriter, r *http.Request) {
	// session 是已认证会话；accountID 是单账号路径参数。
	session := auth.SessionFromContext(r.Context())
	// accountID 只用于授权和版本作用域。
	accountID := chi.URLParam(r, "account_id")
	// snapshot、loadErr 是专用授权结果和读取错误。
	snapshot, loadErr := s.applicationServiceSet().cookieExchange.Snapshot(r.Context(), session.UserID, accountID)
	if loadErr != nil {
		cookieExchangeError(w, r, loadErr)
		return
	}
	// result 只复制专用 Cookie 字段，不引用数据库模型。
	result := exchangeSnapshotDTO{AccountID: accountID, Version: snapshot.Version, Complete: true, Cookies: make([]exchangeCookieDTO, 0, len(snapshot.Cookies))}
	// cookie 是当前被授权导出的条目。
	for _, cookie := range snapshot.Cookies {
		result.Cookies = append(result.Cookies, exchangeCookieDTO{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Expires: cookie.Expires, HTTPOnly: cookie.HTTPOnly, Secure: cookie.Secure, SameSite: cookie.SameSite, PartitionKey: cookie.PartitionKey})
	}
	writeJSON(w, http.StatusOK, result)
}

// cookieExchangeUpdates 用 w、r 限制体积和来源，调用提交用例，错误不输出输入和底层文本。
func (s *Server) cookieExchangeUpdates(w http.ResponseWriter, r *http.Request) {
	// body、readErr 用硬字节上限判断 413，包括未知长度或尾随数据。
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, exchangeBodyLimit))
	if readErr != nil {
		// oversized 是超限专用错误，仅用于状态码选择。
		var oversized *http.MaxBytesError
		if errors.As(readErr, &oversized) {
			writeErrRequest(w, r, http.StatusRequestEntityTooLarge, "Cookie 更新请求过大")
		} else {
			writeErrRequest(w, r, http.StatusBadRequest, "无法读取 Cookie 更新")
		}
		return
	}
	// request 是严格具名输入，未知字段和多个 JSON 对象都拒绝。
	var request exchangeUpdatesDTO
	// decoder 不把原始解析错误回传，避免泄露头部明文。
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		writeErrRequest(w, r, http.StatusBadRequest, "Cookie 更新格式不合法")
		return
	}
	// trailing 只用于检查首个 JSON 后是否还有内容。
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		writeErrRequest(w, r, http.StatusBadRequest, "Cookie 更新格式不合法")
		return
	}
	// batches、valid 是经过来源、时间和头部边界检查的增量。
	batches, valid := validateExchangeUpdates(request, time.Now().UTC())
	if !valid {
		writeErrRequest(w, r, http.StatusBadRequest, "Cookie 更新来源、时间或边界不合法")
		return
	}
	// session 是认证会话，accountID 是目标账号。
	session := auth.SessionFromContext(r.Context())
	// accountID 在仓储短锁内重新校验归属。
	accountID := chi.URLParam(r, "account_id")
	// result、commitErr 是不含秘密的提交结果和持久化错误。
	result, commitErr := s.applicationServiceSet().cookieExchange.Commit(r.Context(), session.UserID, accountID, request.Version, batches)
	if commitErr != nil {
		cookieExchangeError(w, r, commitErr)
		return
	}
	writeJSON(w, http.StatusOK, exchangeResultDTO{AccountID: accountID, Changed: result.Changed, Version: result.Version, SyncStatus: result.SyncStatus})
}

// validateExchangeUpdates 用 request 和 now 校验固定主机及重放窗口，返回有序 batches 与是否有效，不执行 URL 请求。
func validateExchangeUpdates(request exchangeUpdatesDTO, now time.Time) ([]accountapp.ExchangeBatch, bool) {
	if !exchangeVersionPattern.MatchString(request.Version) || len(request.Responses) < 1 || len(request.Responses) > 32 {
		return nil, false
	}
	// batches 是应用层输入，previous 约束接收顺序。
	batches := make([]accountapp.ExchangeBatch, 0, len(request.Responses))
	// previous 记录前一跳的原接收时刻。
	var previous time.Time
	// response 是当前逐跳输入，URL 仅用于 Cookie 作用域。
	for _, response := range request.Responses {
		// target、urlErr 是 URL 解析结果，不允许把签名或用户信息带入用例。
		target, urlErr := url.Parse(response.URL)
		if urlErr != nil || target.Scheme != "https" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || (target.Port() != "" && target.Port() != "443") {
			return nil, false
		}
		switch target.Host {
		case "h5api.m.goofish.com", "www.goofish.com", "passport.goofish.com", "seller.goofish.com", "h5api.m.goofish.com:443", "www.goofish.com:443", "passport.goofish.com:443", "seller.goofish.com:443":
		default:
			return nil, false
		}
		// received、timeErr 是原时刻，严格要求 UTC 毫秒，拒绝旧响应及倒序。
		received, timeErr := time.Parse("2006-01-02T15:04:05.000Z", response.ReceivedAt)
		if timeErr != nil || received.Format("2006-01-02T15:04:05.000Z") != response.ReceivedAt || received.Before(now.Add(-10*time.Minute)) || received.After(now.Add(30*time.Second)) || received.Before(previous) || len(response.SetCookies) < 1 || len(response.SetCookies) > 128 {
			return nil, false
		}
		// header 是当前单条 Set-Cookie，协议拒绝的 Cookie 留给现有 Jar 解析器处理。
		for _, header := range response.SetCookies {
			if len(header) < 1 || len(header) > 8192 || strings.ContainsAny(header, "\r\n") {
				return nil, false
			}
		}
		previous = received
		batches = append(batches, accountapp.ExchangeBatch{URL: response.URL, ReceivedAt: received, SetCookies: response.SetCookies})
	}
	return batches, true
}

// cookieExchangeError 将 err 映射为 w、r 的统一错误，禁止传递底层秘密文本。
func cookieExchangeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, accountapp.ErrForbidden):
		writeErrRequest(w, r, http.StatusForbidden, "无权访问该账号")
	case errors.Is(err, accountapp.ErrNotFound):
		writeErrRequest(w, r, http.StatusNotFound, "账号不存在")
	case errors.Is(err, accountapp.ErrCredentialConflict):
		writeErrCode(w, http.StatusConflict, "credential_conflict", "凭证版本冲突", middleware.GetReqID(r.Context()))
	case errors.Is(err, accountapp.ErrCookieSnapshotUnavailable):
		writeErrCode(w, http.StatusConflict, "cookie_snapshot_unavailable", "缺少完整 Cookie 快照", middleware.GetReqID(r.Context()))
	default:
		writeErrRequest(w, r, http.StatusInternalServerError, "Cookie 交换失败")
	}
}
