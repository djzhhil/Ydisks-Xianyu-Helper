package composition

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/adapter"
	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
)

// TestCookieExchangeComposition 用 t 验证专用构造连接真实仓储，缺少运行实例时提交成功但不启动账号。
func TestCookieExchangeComposition(t *testing.T) {
	// ctx 是隔离数据库调用上下文。
	ctx := context.Background()
	// database、_、openErr 是已迁移数据库及打开错误。
	database, _, openErr := db.Open(ctx, filepath.Join(t.TempDir(), "exchange.db"))
	if openErr != nil {
		t.Fatal("数据库打开失败")
	}
	defer database.Close()
	// store 是既有编码器和凭证锁的唯一拥有者。
	store := db.NewStore(database, db.DialectSQLite)
	// created、createErr 是虚构用户的创建结果。
	created, createErr := store.Users.Create(ctx, "fixture", "fixture@example.test", "fixture-password")
	if createErr != nil || !created {
		t.Fatal("创建用户失败")
	}
	if store.Cookies.Save(ctx, "fixture", "sid=old", 1) != nil {
		t.Fatal("创建账号失败")
	}
	if store.Cookies.UpdateRenewalCookie(ctx, "fixture", "sid=old", cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{{Name: "sid", Value: "old", Domain: ".goofish.com", Path: "/"}}), 1) != nil {
		t.Fatal("创建完整快照失败")
	}
	// dependencies、dependencyErr 由既有账号工厂提供窄仓储。
	dependencies, dependencyErr := adapter.NewAccountDependencies(store)
	if dependencyErr != nil {
		t.Fatal("构造窄仓储失败")
	}
	// service 是组合根构造出的完整用例，运行时和日志允许缺省。
	service := newCookieExchangeService(Dependencies{AccountDependencies: dependencies}, accountapp.NewRuntimeService(nil, nil))
	// snapshot、loadErr 是通过真实授权边界读取的快照。
	snapshot, loadErr := service.Snapshot(ctx, 1, "fixture")
	if loadErr != nil || len(snapshot.Cookies) != 1 {
		t.Fatal("专用服务未连接权威仓储")
	}
	// result、commitErr 验证构造链保留完整版本、增量合并和离线状态。
	result, commitErr := service.Commit(ctx, 1, "fixture", snapshot.Version, []accountapp.ExchangeBatch{{URL: "https://www.goofish.com/im", ReceivedAt: time.Now(), SetCookies: []string{"sid=new; Domain=.goofish.com; Path=/"}}})
	if commitErr != nil || !result.Changed || result.SyncStatus != "not_running" {
		t.Fatal("组合根应提交并保持离线，不启用账号")
	}
}

// TestCookieExchangeReporter 用 t 验证可选日志接线不丢失非敏感保存后失败阶段，缺少 logger 时安全无副作用。
func TestCookieExchangeReporter(t *testing.T) {
	cookieExchangeReporter(nil)("Cookie 交换已保存，运行时同步失败")
	// output 捕获独立组合层回调的固定阶段。
	var output bytes.Buffer
	cookieExchangeReporter(slog.New(slog.NewTextHandler(&output, nil)))("Cookie 交换已保存，运行时同步失败")
	if !strings.Contains(output.String(), "Cookie 交换已保存，运行时同步失败") {
		t.Fatal("保存后失败阶段未传入组合根日志")
	}
}
