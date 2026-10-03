package account

import (
	"context"
	"errors"
	"time"
)

// ErrCookieSnapshotUnavailable 表示必须先登录以取得非空完整快照。
var ErrCookieSnapshotUnavailable = errors.New("缺少完整 Cookie 快照")

// ExchangeCookie 是专用授权导出模型；明文禁止进入日志及通用摘要。
type ExchangeCookie struct {
	// Name 是平台 Cookie 名称。
	Name string
	// Value 是仅允许授权导出的明文。
	Value string
	// Domain 保留主机或域作用域。
	Domain string
	// Path 保留请求路径作用域。
	Path string
	// Expires 是 Unix 秒；非正数表示会话 Cookie。
	Expires float64
	// HTTPOnly 保留脚本不可读属性。
	HTTPOnly bool
	// Secure 保留仅 HTTPS 属性。
	Secure bool
	// SameSite 保留浏览器跨站限制。
	SameSite string
	// PartitionKey 是分区站点；空值表示不分区。
	PartitionKey string
}

// ExchangeSnapshot 是请求生命周期内的专用导出结果。
type ExchangeSnapshot struct {
	// Version 是完整状态指纹，不证明登录有效。
	Version string
	// Cookies 保存非空完整快照明文，禁止日志记录。
	Cookies []ExchangeCookie
}

// ExchangeBatch 保存单跳响应增量，不包含业务正文或签名。
type ExchangeBatch struct {
	// URL 是去掉 query 和 fragment 的当跳 HTTPS 地址。
	URL string
	// ReceivedAt 是接收时刻，Max-Age 从此时起算。
	ReceivedAt time.Time
	// SetCookies 是逐条响应头明文，禁止日志记录。
	SetCookies []string
}

// ExchangeCommit 是提交后的非敏感结果。
type ExchangeCommit struct {
	// Changed 表示完整凭证状态是否改变。
	Changed bool
	// Version 是提交后的状态指纹。
	Version string
	// SyncStatus 是保存后同步状态。
	SyncStatus string
}

// CookieExchangeRepository 由应用用例消费，归属和版本必须在同账号短锁内复核。
type CookieExchangeRepository interface {
	// Snapshot 按上下文、用户和账号读取授权快照。
	Snapshot(context.Context, int64, string) (ExchangeSnapshot, error)
	// Commit 按上下文、用户、账号、旧版本和有序增量提交；返回前释放锁。
	Commit(context.Context, int64, string, string, []ExchangeBatch) (ExchangeCommit, error)
}

// CookieExchangeService 编排提交后同步，不负责登录、续期或启动账号。
type CookieExchangeService struct {
	// repository 拥有持久化和凭证锁。
	repository CookieExchangeRepository
	// running 只查询现有实例，禁止启动账号。
	running func(string) bool
	// sync 通过既有运行时端口重读权威凭证，不传旧 Cookie。
	sync func(context.Context, string, string) error
	// report 只记录固定阶段，不接收可能含秘密的底层错误。
	report func(string)
}

// NewCookieExchangeService 根据 repository 和可选 running、sync、report 回调构造用例，缺少运行时保持离线。
func NewCookieExchangeService(repository CookieExchangeRepository, running func(string) bool, sync func(context.Context, string, string) error, report func(string)) *CookieExchangeService {
	return &CookieExchangeService{repository: repository, running: running, sync: sync, report: report}
}

// Snapshot 按 ctx、userID、accountID 读取授权快照，仓储错误向 transport 映射。
func (s *CookieExchangeService) Snapshot(ctx context.Context, userID int64, accountID string) (ExchangeSnapshot, error) {
	return s.repository.Snapshot(ctx, userID, accountID)
}

// Commit 用 ctx、userID、accountID、version 和 batches 提交；释放锁后同步，失败不回滚已保存凭证。
func (s *CookieExchangeService) Commit(ctx context.Context, userID int64, accountID, version string, batches []ExchangeBatch) (ExchangeCommit, error) {
	// result、commitErr 是非敏感结果和持久化错误；成功后不再返回提交失败。
	result, commitErr := s.repository.Commit(ctx, userID, accountID, version, batches)
	if commitErr != nil {
		return ExchangeCommit{}, commitErr
	}
	result.SyncStatus = "not_needed"
	if !result.Changed {
		return result, nil
	}
	result.SyncStatus = "not_running"
	if s.running == nil || !s.running(accountID) {
		return result, nil
	}
	result.SyncStatus = "failed"
	if s.sync != nil {
		// syncErr 只用于判断结果，禁止输出底层文本。
		syncErr := s.sync(ctx, accountID, "")
		if syncErr == nil {
			result.SyncStatus = "synced"
		}
	}
	if result.SyncStatus == "failed" && s.report != nil {
		s.report("Cookie 交换已保存，运行时同步失败")
	}
	return result, nil
}
