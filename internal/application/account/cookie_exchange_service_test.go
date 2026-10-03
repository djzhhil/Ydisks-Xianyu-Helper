package account

import (
	"context"
	"errors"
	"testing"
)

// exchangeRepositoryFake 是仅返回非敏感状态的用例仓储替身。
type exchangeRepositoryFake struct {
	// result 是预置提交结果。
	result ExchangeCommit
	// err 是读取或提交故障，测试不打印秘密。
	err error
}

// Snapshot 按 ctx、userID、accountID 返回专用夹具；本替身不执行网络和持久化。
func (f exchangeRepositoryFake) Snapshot(ctx context.Context, userID int64, accountID string) (ExchangeSnapshot, error) {
	return ExchangeSnapshot{Version: "fixture"}, f.err
}

// Commit 按 ctx、userID、accountID、version、batches 返回预置提交状态。
func (f exchangeRepositoryFake) Commit(ctx context.Context, userID int64, accountID, version string, batches []ExchangeBatch) (ExchangeCommit, error) {
	return f.result, f.err
}

// TestCookieExchangeServiceSyncStates 用 t 验证无变化、无实例、同步成功、失败及已保存后的取消语义。
func TestCookieExchangeServiceSyncStates(t *testing.T) {
	// testCase 是当前用例状态组合，不携带 Cookie。
	for _, testCase := range []struct {
		// name 是失败诊断场景。
		name string
		// changed 表示数据库是否已经写入。
		changed bool
		// running 表示实例存在性，禁止启动。
		running bool
		// syncErr 表示运行时错误。
		syncErr error
		// expected 是可重放判断所需的固定同步状态。
		expected string
	}{
		{name: "unchanged", expected: "not_needed"},
		{name: "offline", changed: true, expected: "not_running"},
		{name: "synced", changed: true, running: true, expected: "synced"},
		{name: "failed", changed: true, running: true, syncErr: errors.New("fixture-sensitive"), expected: "failed"},
		{name: "canceled-after-save", changed: true, running: true, syncErr: context.Canceled, expected: "failed"},
	} {
		// testCase 副本保持子测试闭包隔离。
		testCase := testCase
		// t 是当前同步状态子测试句柄。
		t.Run(testCase.name, func(t *testing.T) {
			// syncCalls、reports 统计同步及固定非敏感日志回调。
			syncCalls, reports := 0, 0
			// service 使用可观测回调，运行时参数必须为空以复读权威状态。
			service := NewCookieExchangeService(exchangeRepositoryFake{result: ExchangeCommit{Changed: testCase.changed, Version: "saved"}}, func(accountID string) bool { return testCase.running }, func(ctx context.Context, accountID, value string) error {
				// ctx、accountID 是调用上下文和目标；value 必须为空，防止旧参数回写。
				if value != "" {
					t.Fatal("同步不应传递旧 Cookie")
				}
				syncCalls++
				return testCase.syncErr
			}, func(message string) { // message 必须是固定非敏感阶段。
				if message != "Cookie 交换已保存，运行时同步失败" {
					t.Fatal("错误日志应脱敏")
				}
				reports++
			})
			// result、err 是已保存结果，后续失败也不能返回提交错误。
			result, err := service.Commit(context.Background(), 1, "account", "version", nil)
			if err != nil || result.SyncStatus != testCase.expected || result.Version != "saved" {
				t.Fatal("同步状态映射错误")
			}
			if (!testCase.changed || !testCase.running) && syncCalls != 0 {
				t.Fatal("不应启动或同步离线实例")
			}
			if (testCase.expected == "failed") != (reports == 1) {
				t.Fatal("同步失败日志语义不正确")
			}
		})
	}
	// repositoryErr 是预置提交失败，必须在同步前返回。
	repositoryErr := errors.New("fixture persistence failed")
	// service 禁止持久化失败后进入同步。
	service := NewCookieExchangeService(exchangeRepositoryFake{err: repositoryErr}, nil, nil, nil)
	// err 是保存失败，不能进入运行时同步。
	if _, err := service.Commit(context.Background(), 1, "account", "version", nil); !errors.Is(err, repositoryErr) {
		t.Fatal("持久化失败应向上返回")
	} // err 是仓储错误。
	if _, err := service.Snapshot(context.Background(), 1, "account"); !errors.Is(err, repositoryErr) {
		t.Fatal("读取失败应向上返回")
	} // err 是快照查询错误。
	// missingSync 模拟未装配可选运行时同步回调。
	missingSync := NewCookieExchangeService(exchangeRepositoryFake{result: ExchangeCommit{Changed: true}}, func(accountID string) bool { return true }, nil, nil) // accountID 是当前实例查询目标。
	if result, err := missingSync.Commit(context.Background(), 1, "account", "version", nil); err != nil || result.SyncStatus != "failed" {
		t.Fatal("缺同步回调应报告已保存但失败")
	} // result、err 是同步回调缺失结果。
}
