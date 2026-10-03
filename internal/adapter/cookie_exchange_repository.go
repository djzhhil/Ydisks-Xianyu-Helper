package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
)

// CookieExchangeRepository 复用 Store 短锁和加密仓储；锁内只做本地凭证操作，不调用运行时或网络。
type CookieExchangeRepository struct {
	// store 拥有本进程账号凭证锁、编码器和缓存。
	store *db.Store
	// logger 只记录固定原因，禁止输出底层错误和 Cookie。
	logger *slog.Logger
}

// NewCookieExchangeRepository 用 store 和可选 logger 创建独立适配器。
func NewCookieExchangeRepository(store *db.Store, logger *slog.Logger) *CookieExchangeRepository {
	return &CookieExchangeRepository{store: store, logger: logger}
}

// load 在调用者持有凭证锁期间先查非敏感归属，再解密账号 data 和 snapshot；失败不导出任何秘密。
func (r *CookieExchangeRepository) load(ctx context.Context, userID int64, accountID string) (db.CookiePlatformRuntimeData, []cookierefresh.BrowserCookie, error) {
	if r == nil || r.store == nil || r.store.Cookies == nil {
		return db.CookiePlatformRuntimeData{}, nil, errors.New("账号 Cookie 交换仓储未初始化")
	}
	// ownerID、ownerErr 只查询归属，不读取秘密。
	ownerID, ownerErr := r.store.Cookies.GetOwnerID(ctx, accountID)
	if errors.Is(ownerErr, db.ErrNotFound) {
		return db.CookiePlatformRuntimeData{}, nil, accountapp.ErrNotFound
	}
	if ownerErr != nil {
		return db.CookiePlatformRuntimeData{}, nil, ownerErr
	}
	if userID <= 0 || ownerID != userID {
		return db.CookiePlatformRuntimeData{}, nil, accountapp.ErrForbidden
	}
	// data、loadErr 保存窄凭证视图和读取失败；明文只在本次操作内存在。
	data, loadErr := r.store.Cookies.GetCookiePlatformRuntimeData(ctx, accountID)
	if errors.Is(loadErr, db.ErrNotFound) {
		return data, nil, accountapp.ErrNotFound
	}
	if loadErr != nil {
		return data, nil, loadErr
	}
	if data.UserID != userID {
		return db.CookiePlatformRuntimeData{}, nil, accountapp.ErrForbidden
	}
	// snapshot、complete 表示已归一化的完整 Jar 和完整标记。
	snapshot, complete := cookierefresh.SnapshotFromMetadataOK(data.MetadataJSON)
	if !complete || len(snapshot) == 0 {
		return data, nil, accountapp.ErrCookieSnapshotUnavailable
	}
	return data, snapshot, nil
}

// exchangeVersion 对 accountID、flat、完整标记和有序 snapshot 计算稳定指纹，排除刷新时间与无关 metadata。
func exchangeVersion(accountID, flat string, snapshot []cookierefresh.BrowserCookie) string {
	// state 是不可导出的哈希输入；全部字段均可稳定 JSON 编码。
	state := struct {
		// AccountID 绑定账号作用域。
		AccountID string
		// Flat 保留消息页规范字符串。
		Flat string
		// Complete 明确快照存在；包括更新后空 Jar。
		Complete bool
		// Snapshot 保留顺序与全部属性。
		Snapshot []cookierefresh.BrowserCookie
	}{accountID, flat, true, cookierefresh.NormalizeSnapshot(snapshot)}
	// encoded 是临时哈希输入；固定模型不含不可编码字段。
	encoded, _ := json.Marshal(state)
	// digest 是非敏感状态摘要。
	digest := sha256.Sum256(encoded)
	return "v1:" + hex.EncodeToString(digest[:])
}

// Snapshot 在短锁内按 ctx、userID、accountID 复核归属并返回专用导出模型；不触发平台请求。
func (r *CookieExchangeRepository) Snapshot(ctx context.Context, userID int64, accountID string) (accountapp.ExchangeSnapshot, error) {
	if r == nil || r.store == nil {
		return accountapp.ExchangeSnapshot{}, errors.New("账号 Cookie 交换仓储未初始化")
	}
	// unlock 只覆盖本地查询和 DTO 转换，返回前释放。
	unlock := r.store.LockAccountCredentials(accountID)
	defer unlock()
	// data、snapshot、loadErr 保存授权凭证、完整 Jar 和读取错误。
	data, snapshot, loadErr := r.load(ctx, userID, accountID)
	if loadErr != nil {
		return accountapp.ExchangeSnapshot{}, loadErr
	}
	// result 是仅允许交换接口序列化的专用模型。
	result := accountapp.ExchangeSnapshot{Version: exchangeVersion(accountID, data.Value, snapshot), Cookies: make([]accountapp.ExchangeCookie, 0, len(snapshot))}
	// cookie 是当前完整 Jar 条目，不进入普通摘要。
	for _, cookie := range snapshot {
		result.Cookies = append(result.Cookies, accountapp.ExchangeCookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Expires: cookie.Expires, HTTPOnly: cookie.HTTPOnly, Secure: cookie.Secure, SameSite: cookie.SameSite, PartitionKey: cookie.PartitionKey})
	}
	return result, nil
}

// Commit 在 ctx 对应本地短锁内复核 userID、accountID 和 version，按 batches 原时刻重放；保存及缓存清理完成后释放锁。
func (r *CookieExchangeRepository) Commit(ctx context.Context, userID int64, accountID, version string, batches []accountapp.ExchangeBatch) (accountapp.ExchangeCommit, error) {
	if r == nil || r.store == nil {
		return accountapp.ExchangeCommit{}, errors.New("账号 Cookie 交换仓储未初始化")
	}
	// unlock 不跨越运行时同步，避免同步重取凭证锁死锁。
	unlock := r.store.LockAccountCredentials(accountID)
	defer unlock()
	// data、snapshot、loadErr 保存锁内的最新凭证及完整快照。
	data, snapshot, loadErr := r.load(ctx, userID, accountID)
	if loadErr != nil {
		return accountapp.ExchangeCommit{}, loadErr
	}
	// current 是完整状态版本，不依赖秒级刷新时间。
	current := exchangeVersion(accountID, data.Value, snapshot)
	if current != version {
		return accountapp.ExchangeCommit{}, accountapp.ErrCredentialConflict
	}
	// updated 保留 Cookie 创建顺序；每跳按原接收时刻解释 Max-Age。
	updated := snapshot
	// batch 是当前接收顺序中的响应，分区站点由 Helper 固定。
	for _, batch := range batches {
		updated = cookierefresh.ApplySetCookies(updated, batch.URL, batch.SetCookies, batch.ReceivedAt, "https://goofish.com")
	}
	if slices.Equal(snapshot, updated) {
		return accountapp.ExchangeCommit{Version: current}, nil
	}
	// flat 是 Helper /im 当前时刻的规范请求头，不携带其他路径 Cookie。
	flat := cookierefresh.CookieHeaderForURL(updated, "https://www.goofish.com/im", time.Now())
	// next 是提交后指纹；纯属性变化也算凭证变化。
	next := exchangeVersion(accountID, flat, updated)
	// saveErr 保存加密更新结果；该 API 仅更新现有账号，不创建删除账号。
	saveErr := r.store.Cookies.UpdateRenewalCookie(ctx, accountID, flat, cookierefresh.MetadataWithSnapshot(data.MetadataJSON, updated), time.Now().Unix())
	if errors.Is(saveErr, db.ErrNotFound) {
		return accountapp.ExchangeCommit{}, accountapp.ErrNotFound
	}
	if saveErr != nil {
		return accountapp.ExchangeCommit{}, saveErr
	}
	// clearErr 是非阻断缓存清理结果，不输出可能包含秘密的底层文本。
	clearErr := r.store.Tokens.Clear(ctx, accountID)
	if clearErr != nil && r.logger != nil {
		r.logger.Warn("Cookie 交换已保存，旧连接 Token 清理失败")
	}
	return accountapp.ExchangeCommit{Changed: true, Version: next}, nil
}
