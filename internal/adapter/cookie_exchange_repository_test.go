package adapter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
)

// TestCookieExchangeRepositoryReplay 用 t 验证多跳、同名作用域、分区、原时刻有效期、删除、完整 metadata 与加密更新。
func TestCookieExchangeRepositoryReplay(t *testing.T) {
	// store、cleanup 是隔离数据库与释放责任。
	t.Setenv("XIANYU_DATA_KEY", "cookie-exchange-fixture-key")
	// store、cleanup 是加密夹具数据库及释放责任。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// ctx 是本地数据库上下文。
	ctx := context.Background()
	// received 是一分钟前的响应时刻，Max-Age 不从提交时起算。
	received := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	// original 是两种路径的同名 Cookie。
	original := []cookierefresh.BrowserCookie{{Name: "sid", Value: "root", Domain: ".goofish.com", Path: "/"}, {Name: "sid", Value: "path", Domain: ".goofish.com", Path: "/h5"}}
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=root", cookierefresh.MetadataWithSnapshot(`{"device":"keep"}`, original), 1) != nil {
		t.Fatal("初始化失败")
	}
	// repository 是专用交换适配器。
	repository := NewCookieExchangeRepository(store, nil)
	// before、readErr 是提交前完整版本和读取错误。
	before, readErr := repository.Snapshot(ctx, 1, "cid")
	if readErr != nil {
		t.Fatal("读取快照失败")
	}
	// batches 模拟 Reader 捕获的逐跳头部，包含合法分区与域拒绝。
	batches := []accountapp.ExchangeBatch{
		{URL: "https://h5api.m.goofish.com/h5/a/", ReceivedAt: received, SetCookies: []string{"sid=first; Domain=.goofish.com; Path=/; HttpOnly; Secure", "age=v; Domain=.goofish.com; Path=/; Max-Age=3600", "part=p; Path=/; Secure; SameSite=None; Partitioned", "bad=secret; Domain=evil.com; Path=/"}},
		{URL: "https://www.goofish.com/im", ReceivedAt: received.Add(time.Second), SetCookies: []string{"sid=final; Domain=.goofish.com; Path=/; HttpOnly; Secure", "sid=; Domain=.goofish.com; Path=/h5; Max-Age=0"}},
	}
	// result、commitErr 是本地提交结果。
	result, commitErr := repository.Commit(ctx, 1, "cid", before.Version, batches)
	if commitErr != nil || !result.Changed || result.Version == before.Version {
		t.Fatal("多跳提交失败")
	}
	// stored、storedErr 是权威存储窄视图，不在失败信息中打印。
	stored, storedErr := store.Cookies.GetCookieRuntimeData(ctx, "cid")
	if storedErr != nil || !strings.Contains(stored.MetadataJSON, `"device":"keep"`) || strings.Contains(stored.Value, "part=") || strings.Contains(stored.Value, "sid=path") {
		t.Fatal("作用域或 metadata 保存不正确")
	}
	// snapshot 是提交后的完整 Jar。
	snapshot := cookierefresh.SnapshotFromMetadata(stored.MetadataJSON)
	if len(snapshot) != 3 {
		t.Fatal("域拒绝或精确删除未生效")
	}
	// cookie 是当前持久化条目，只比较属性不打印明文。
	for _, cookie := range snapshot {
		switch cookie.Name {
		case "sid":
			if cookie.Value != "final" || !cookie.HTTPOnly || !cookie.Secure {
				t.Fatal("多跳顺序或属性丢失")
			}
		case "age":
			if cookie.Expires != float64(received.Unix()+3600) {
				t.Fatal("Max-Age 未从原时刻起算")
			}
		case "part":
			if cookie.PartitionKey != "https://goofish.com" {
				t.Fatal("固定分区站点丢失")
			}
		}
	}
	// repeatErr 是旧版本重放拒绝结果。
	if _, repeatErr := repository.Commit(ctx, 1, "cid", before.Version, batches); !errors.Is(repeatErr, accountapp.ErrCredentialConflict) {
		t.Fatal("重复提交应冲突")
	} // repeatErr 表示旧版本再次提交的冲突。
	// unchanged 采用无效 Cookie 头部，不能改变刷新时间或版本。
	unchanged, noChangeErr := repository.Commit(ctx, 1, "cid", result.Version, []accountapp.ExchangeBatch{{URL: batches[0].URL, ReceivedAt: received, SetCookies: []string{"bad header"}}})
	if noChangeErr != nil || unchanged.Changed || unchanged.Version != result.Version {
		t.Fatal("无效头部不能写回")
	}
	// sameSecondMetadata 是同秒属性变化，必须改变版本。
	snapshot[0].Secure = false
	// sameSecondMetadata 保留同秒修订时间并改变安全属性。
	sameSecondMetadata := cookierefresh.MetadataWithSnapshot(stored.MetadataJSON, snapshot)
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", stored.Value, sameSecondMetadata, stored.LastRefreshAt) != nil {
		t.Fatal("写入同秒变化失败")
	}
	// conflictErr 是同秒属性改变后的版本拒绝。
	if _, conflictErr := repository.Commit(ctx, 1, "cid", result.Version, batches); !errors.Is(conflictErr, accountapp.ErrCredentialConflict) {
		t.Fatal("同秒属性变化必须冲突")
	} // conflictErr 是属性变化后的提交结果。
}

// TestCookieExchangeRepositoryConcurrency 用 t 验证同版本并发请求只有一个提交者，删除及归属变更在锁内复核。
func TestCookieExchangeRepositoryConcurrency(t *testing.T) {
	// store、cleanup 是隔离的权威存储。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// ctx 是并发测试共同上下文。
	ctx := context.Background()
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=old", cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{{Name: "sid", Value: "old", Domain: ".goofish.com", Path: "/"}}), 1) != nil {
		t.Fatal("初始化失败")
	}
	// repository 是共享短锁适配器。
	repository := NewCookieExchangeRepository(store, nil)
	// before、loadErr 是共同初始版本。
	before, loadErr := repository.Snapshot(ctx, 1, "cid")
	if loadErr != nil {
		t.Fatal("读取失败")
	}
	// batches 是同一旧版本增量。
	batches := []accountapp.ExchangeBatch{{URL: "https://www.goofish.com/im", ReceivedAt: time.Now(), SetCookies: []string{"sid=new; Domain=.goofish.com; Path=/"}}}
	// outcomes 收集两个请求的结果；仅发送者写入，不关闭，主测试等待两个值。
	outcomes := make(chan error, 2)
	// wait 等待并发提交，goroutine 生命周期仅属于此测试。
	var wait sync.WaitGroup
	// index 标识两个相同旧版本请求。
	for index := 0; index < 2; index++ {
		wait.Add(1)
		// goroutine 仅执行短本地提交，返回前由 wait 等待。
		go func() {
			defer wait.Done()
			// commitErr 是此请求的提交错误。
			_, commitErr := repository.Commit(ctx, 1, "cid", before.Version, batches)
			outcomes <- commitErr
		}()
	}
	wait.Wait()
	// successes、conflicts 统计提交与版本拒绝。
	successes, conflicts := 0, 0
	// index 标识待收取的两次结果。
	for index := 0; index < 2; index++ {
		// outcome 是一次提交结果，不含 Cookie。
		outcome := <-outcomes
		if outcome == nil {
			successes++
		} else if errors.Is(outcome, accountapp.ErrCredentialConflict) {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("同版本并发必须只保存一次")
	}
	// unlock 模拟删除流程先持有凭证锁。
	unlock := store.LockAccountCredentials("cid")
	// started 确认读取请求已被派发，主测试随后在锁内删除。
	started := make(chan struct{})
	// deletedResult 是等待锁后的请求结果。
	deletedResult := make(chan error, 1)
	// goroutine 属于本测试，主线程读取 deletedResult 后结束。
	go func() {
		close(started)
		// snapshotErr 是获得短锁后复核已删除账号的结果。
		_, snapshotErr := repository.Snapshot(ctx, 1, "cid")
		deletedResult <- snapshotErr
	}() // snapshotErr 是删除后读取结果。
	<-started
	if store.Cookies.Delete(ctx, "cid") != nil {
		t.Fatal("删除失败")
	}
	unlock()
	if !errors.Is(<-deletedResult, accountapp.ErrNotFound) {
		t.Fatal("锁内复核应识别已删除账号")
	}
	// commitErr 是删除后提交的拒绝结果。
	if _, commitErr := repository.Commit(ctx, 1, "cid", before.Version, batches); !errors.Is(commitErr, accountapp.ErrNotFound) {
		t.Fatal("不得重新创建已删除账号")
	} // commitErr 是删除后回写结果。
}

// TestCookieExchangeRepositoryFailures 用 t 验证未装配、越权、缺快照、持久化失败和缓存清理失败的语义及日志隔离。
func TestCookieExchangeRepositoryFailures(t *testing.T) {
	// ctx 是本地测试上下文。
	ctx := context.Background()
	// err 表示未装配仓储无法读取快照。
	if _, err := NewCookieExchangeRepository(nil, nil).Snapshot(ctx, 1, "cid"); err == nil {
		t.Fatal("缺少存储应失败")
	} // err 是构造防御结果。
	if _, err := NewCookieExchangeRepository(nil, nil).Commit(ctx, 1, "cid", "", nil); err == nil {
		t.Fatal("缺少存储提交应失败")
	} // err 是构造防御结果。
	// store、cleanup 是独立数据库。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// logs 只收集非敏感缓存清理阶段。
	var logs bytes.Buffer
	// repository 使用可检查的日志输出。
	repository := NewCookieExchangeRepository(store, slog.New(slog.NewTextHandler(&logs, nil)))
	// err 表示当前用户无权导出账号。
	if _, err := repository.Snapshot(ctx, 2, "cid"); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatal("越权应失败")
	} // err 是归属拒绝结果。
	if _, err := repository.Snapshot(ctx, 1, "missing"); !errors.Is(err, accountapp.ErrNotFound) {
		t.Fatal("不存在应失败")
	} // err 是账号缺失结果。
	if _, err := repository.Snapshot(ctx, 1, "cid"); !errors.Is(err, accountapp.ErrCookieSnapshotUnavailable) {
		t.Fatal("扁平账号不能导出")
	} // err 是快照缺失结果。
	// original 是虚构凭证，失败消息不得打印它。
	original := []cookierefresh.BrowserCookie{{Name: "sid", Value: "old", Domain: ".goofish.com", Path: "/"}}
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=old", cookierefresh.MetadataWithSnapshot(`{}`, original), 1) != nil {
		t.Fatal("初始化失败")
	}
	// before、loadErr 是初始快照。
	before, loadErr := repository.Snapshot(ctx, 1, "cid")
	if loadErr != nil {
		t.Fatal("读取失败")
	}
	// batches 是含虚构秘密的更新。
	batches := []accountapp.ExchangeBatch{{URL: "https://www.goofish.com/im", ReceivedAt: time.Now(), SetCookies: []string{"sid=fixture-sensitive; Domain=.goofish.com; Path=/"}}}
	// triggerErr 注入 UPDATE 故障，读取仍正常。
	_, triggerErr := store.DB.ExecContext(ctx, `CREATE TRIGGER exchange_fail BEFORE UPDATE ON cookies BEGIN SELECT RAISE(ABORT, 'fixture-sensitive'); END`)
	if triggerErr != nil {
		t.Fatal("创建故障夹具失败")
	}
	// err 是本地持久化故障，不能伪报提交成功。
	if _, err := repository.Commit(ctx, 1, "cid", before.Version, batches); err == nil {
		t.Fatal("保存失败不得伪报成功")
	} // err 是持久化故障。
	// stored、storedErr 验证失败没有部分凭证更新。
	stored, storedErr := store.Cookies.GetCookieRuntimeData(ctx, "cid")
	if storedErr != nil || stored.Value != "sid=old" {
		t.Fatal("失败必须保留权威凭证")
	}
	// err 是故障夹具清理的数据库结果。
	if _, err := store.DB.ExecContext(ctx, `DROP TRIGGER exchange_fail`); err != nil {
		t.Fatal("移除故障失败")
	} // err 是故障夹具清理结果。
	// brokenTokens 在另一独立关闭数据库上制造缓存失败，不破坏账号存储。
	brokenTokens, tokensCleanup := newAdapterTestStore(t)
	tokensCleanup()
	store.Tokens = brokenTokens.Tokens
	// result、err 是缓存故障但凭证已保存的结果。
	if result, err := repository.Commit(ctx, 1, "cid", before.Version, batches); err != nil || !result.Changed {
		t.Fatal("缓存失败不应回滚提交")
	} // result、err 是已保存但缓存故障的结果。
	if !strings.Contains(logs.String(), "Token 清理失败") || strings.Contains(logs.String(), "fixture-sensitive") {
		t.Fatal("缓存错误日志必须脱敏")
	}
	// closed 仓储查询故障不被当作不存在或成功。
	store.DB.Close()
	// err 是数据库关闭后查询的基础设施故障。
	if _, err := repository.Snapshot(ctx, 1, "cid"); err == nil || errors.Is(err, db.ErrNotFound) {
		t.Fatal("数据库故障必须向上返回")
	} // err 是基础设施查询失败。
}

// TestCookieExchangeSyncAfterUnlock 用 t 验证应用服务在持久化释放锁后才调用同步，并复读最新权威状态。
func TestCookieExchangeSyncAfterUnlock(t *testing.T) {
	// store、cleanup 是本地数据库及释放责任。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// ctx 是带超时的同步预算；cancel 在测试结束释放。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// initial 是虚构完整凭证。
	initial := []cookierefresh.BrowserCookie{{Name: "sid", Value: "old", Domain: ".goofish.com", Path: "/"}}
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=old", cookierefresh.MetadataWithSnapshot(`{}`, initial), 1) != nil {
		t.Fatal("初始化失败")
	}
	// repository 是持有账号短锁的适配器。
	repository := NewCookieExchangeRepository(store, nil)
	// snapshot、loadErr 是提交前版本。
	snapshot, loadErr := repository.Snapshot(ctx, 1, "cid")
	if loadErr != nil {
		t.Fatal("读取失败")
	}
	// service 同步回调重取同账号锁，若提交未释放则会超时。
	service := accountapp.NewCookieExchangeService(repository, func(accountID string) bool { return true }, func(ctx context.Context, accountID, value string) error {
		// unlock 模拟既有运行时重取同账号锁；不持锁调用外部平台。
		unlock := store.LockAccountCredentials(accountID)
		defer unlock()
		// latest、readErr 是数据库权威值，value 不应携带旧凭证。
		latest, readErr := store.Cookies.GetCookieRuntimeData(ctx, accountID)
		if value != "" || readErr != nil || latest.Value != "sid=new" {
			return errors.New("权威状态复读失败")
		}
		return nil
	}, nil) // accountID 是当前实例标识，不启动实例。
	// finished 是测试用例归属的结果通道，主线程等结果后结束。
	finished := make(chan accountapp.ExchangeCommit, 1)
	// goroutine 由主测试等待，不访问任何平台网络。
	go func() {
		// result、commitErr 是同步后的完整结果。
		result, commitErr := service.Commit(ctx, 1, "cid", snapshot.Version, []accountapp.ExchangeBatch{{URL: "https://www.goofish.com/im", ReceivedAt: time.Now(), SetCookies: []string{"sid=new; Domain=.goofish.com; Path=/"}}})
		if commitErr != nil {
			result.SyncStatus = "unexpected_error"
		}
		finished <- result
	}()
	select {
	case result := <-finished: // result 是已经完成短锁释放和同步的状态。
		if result.SyncStatus != "synced" {
			t.Fatal("同步必须复读保存后的权威状态")
		}
	case <-ctx.Done():
		t.Fatal("同步重取凭证锁发生死锁")
	}
}

// TestCookieExchangeVersionAndEmptyJar 用 t 验证指纹包含所有属性和順序、不受无关 metadata 影响，最后一条删除仍正常保存。
func TestCookieExchangeVersionAndEmptyJar(t *testing.T) {
	// store、cleanup 是虚构账号数据库。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// ctx 是数据库调用上下文。
	ctx := context.Background()
	// original 保留同名不同域及创建顺序。
	original := []cookierefresh.BrowserCookie{{Name: "sid", Value: "v", Domain: ".goofish.com", Path: "/"}, {Name: "sid", Value: "v", Domain: "www.goofish.com", Path: "/"}}
	// version 是状态指纹，只比较摘要不输出原始 Cookie。
	version := exchangeVersion("cid", "sid=v", original)
	// mutations 是各种凭证属性变化。
	mutations := [][]cookierefresh.BrowserCookie{append([]cookierefresh.BrowserCookie(nil), original...), {original[1], original[0]}}
	mutations[0][0].HTTPOnly = true
	// changed 是当前不同属性或顺序的快照。
	for _, changed := range mutations {
		if exchangeVersion("cid", "sid=v", changed) == version {
			t.Fatal("指纹漏掉属性或顺序")
		}
	}
	if exchangeVersion("other", "sid=v", original) == version || exchangeVersion("cid", "sid=other", original) == version {
		t.Fatal("指纹漏掉账号或扁平凭证")
	}
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=v", cookierefresh.MetadataWithSnapshot(`{"device":"old"}`, original[:1]), 1) != nil {
		t.Fatal("初始化失败")
	}
	// repository 是权威凭证适配器。
	repository := NewCookieExchangeRepository(store, nil)
	// before、readErr 是初始版本。
	before, readErr := repository.Snapshot(ctx, 1, "cid")
	if readErr != nil {
		t.Fatal("读取失败")
	}
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=v", cookierefresh.MetadataWithSnapshot(`{"device":"new"}`, original[:1]), 999) != nil {
		t.Fatal("写入无关 metadata 失败")
	}
	// after、afterErr 是只改变非凭证属性后的版本。
	after, afterErr := repository.Snapshot(ctx, 1, "cid")
	if afterErr != nil || after.Version != before.Version {
		t.Fatal("版本不应受刷新时间或无关 metadata 影响")
	}
	// result、commitErr 是删除最后一条 Cookie 的结果。
	result, commitErr := repository.Commit(ctx, 1, "cid", before.Version, []accountapp.ExchangeBatch{{URL: "https://www.goofish.com/im", ReceivedAt: time.Now(), SetCookies: []string{"sid=; Domain=.goofish.com; Path=/; Max-Age=0"}}})
	if commitErr != nil || !result.Changed {
		t.Fatal("完整空 Jar 应保存成功")
	}
	// stored、storedErr 是删除后的权威状态。
	stored, storedErr := store.Cookies.GetCookieRuntimeData(ctx, "cid")
	if storedErr != nil || stored.Value != "" {
		t.Fatal("空 Jar 不得退回旧扁平凭证")
	}
	// jar、complete 区分显式完整空 Jar 和历史缺失快照。
	jar, complete := cookierefresh.SnapshotFromMetadataOK(stored.MetadataJSON)
	if !complete || len(jar) != 0 {
		t.Fatal("删除后必须保留完整标记")
	}
	// unavailable 是随后导出的明确拒绝，不能伪造非空凭证。
	_, unavailable := repository.Snapshot(ctx, 1, "cid")
	if !errors.Is(unavailable, accountapp.ErrCookieSnapshotUnavailable) {
		t.Fatal("空 Jar 后导出应 409")
	}
}

// TestCookieExchangeRepositoryEncryptedBoundary 用 t 验证 Cookie 和 metadata 加密、无权用户不解密损坏秘密、合法用户正确收到基础设施错误。
func TestCookieExchangeRepositoryEncryptedBoundary(t *testing.T) {
	t.Setenv("XIANYU_DATA_KEY", "cookie-exchange-boundary-fixture")
	// store、cleanup 是启用原编码器的隔离数据库。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// ctx 是本地凭证操作上下文。
	ctx := context.Background()
	if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=fixture-sensitive", cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{{Name: "sid", Value: "fixture-sensitive", Domain: ".goofish.com", Path: "/"}}), 1) != nil {
		t.Fatal("加密写入失败")
	}
	// encodedValue、encodedMetadata 是数据库原始密文，禁止在断言中输出。
	var encodedValue, encodedMetadata string
	if store.DB.QueryRowContext(ctx, `SELECT value,metadata_json FROM cookies WHERE id='cid'`).Scan(&encodedValue, &encodedMetadata) != nil || !strings.HasPrefix(encodedValue, "enc:v1:") || !strings.HasPrefix(encodedMetadata, "enc:v1:") || strings.Contains(encodedValue+encodedMetadata, "fixture-sensitive") {
		t.Fatal("未复用原凭证编码器")
	}
	// repository 是严格先授权再解密的专用适配器。
	repository := NewCookieExchangeRepository(store, nil)
	// corruptErr 只损坏虚构夹具，不触碰真实数据。
	_, corruptErr := store.DB.ExecContext(ctx, `UPDATE cookies SET metadata_json='enc:v1:broken' WHERE id='cid'`)
	if corruptErr != nil {
		t.Fatal("损坏夹具创建失败")
	}
	// forbiddenErr 应在读取加密 metadata 之前返回。
	_, forbiddenErr := repository.Snapshot(ctx, 2, "cid")
	if !errors.Is(forbiddenErr, accountapp.ErrForbidden) {
		t.Fatal("无权用户不应解密秘密")
	}
	// loadErr 是合法用户发现的解密错误，transport 负责脱敏。
	_, loadErr := repository.Snapshot(ctx, 1, "cid")
	if loadErr == nil || errors.Is(loadErr, accountapp.ErrCookieSnapshotUnavailable) {
		t.Fatal("解密故障不得伪报缺失快照")
	}
	// missingCookiesStore 保留 Store 但缺少 Cookies，验证仓储装配防御。
	missingCookiesStore := db.NewStore(store.DB, db.DialectSQLite)
	missingCookiesStore.Cookies = nil
	// err 是缺少窄仓储时的构造防御结果。
	if _, err := NewCookieExchangeRepository(missingCookiesStore, nil).Snapshot(ctx, 1, "cid"); err == nil {
		t.Fatal("缺少 Cookie 仓储应拒绝")
	} // err 是窄仓储装配故障。
}

// TestCookieExchangeOwnershipAfterWaiting 用 t 验证等待凭证锁的读取和提交均重新检查当前归属，不信任请求开始时的所有者。
func TestCookieExchangeOwnershipAfterWaiting(t *testing.T) {
	// write 表示当前测试读取或提交两种等待锁路径。
	for _, write := range []bool{false, true} {
		// store、cleanup 是每个场景独立的数据库。
		store, cleanup := newAdapterTestStore(t)
		// ctx 是测试本地数据库上下文。
		ctx := context.Background()
		// userErr 是创建第二所有者的结果。
		_, userErr := store.Users.Create(ctx, "new-owner", "new-owner@example.test", "fixture-password")
		if userErr != nil {
			cleanup()
			t.Fatal("创建新所有者失败")
		}
		if store.Cookies.UpdateRenewalCookie(ctx, "cid", "sid=old", cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{{Name: "sid", Value: "old", Domain: ".goofish.com", Path: "/"}}), 1) != nil {
			cleanup()
			t.Fatal("初始化失败")
		}
		// repository 是共享当前 Store 凭证锁的适配器。
		repository := NewCookieExchangeRepository(store, nil)
		// before、loadErr 是归属变化之前的合法版本。
		before, loadErr := repository.Snapshot(ctx, 1, "cid")
		if loadErr != nil {
			cleanup()
			t.Fatal("读取初始版本失败")
		}
		// unlock 模拟合法归属转换持有账号锁。
		unlock := store.LockAccountCredentials("cid")
		// ready 标记操作已派发；finished 由主测试收取后结束 goroutine 生命周期。
		ready := make(chan struct{})
		// finished 接收归属复核的非敏感错误。
		finished := make(chan error, 1)
		// goroutine 只执行本地短锁操作，主测试等待结果，不调用平台。
		go func() {
			close(ready)
			// operationErr 是锁内归属复核结果。
			var operationErr error
			if write {
				_, operationErr = repository.Commit(ctx, 1, "cid", before.Version, []accountapp.ExchangeBatch{{URL: "https://www.goofish.com/im", ReceivedAt: time.Now(), SetCookies: []string{"sid=new; Domain=.goofish.com; Path=/"}}})
			} else {
				_, operationErr = repository.Snapshot(ctx, 1, "cid")
			}
			finished <- operationErr
		}()
		<-ready
		// ownerErr 是在同账号短锁内完成的夹具归属转换。
		_, ownerErr := store.DB.ExecContext(ctx, `UPDATE cookies SET user_id=2 WHERE id='cid'`)
		unlock()
		// forbidden 是随后取得锁的操作看到的最新归属。
		forbidden := <-finished
		if ownerErr != nil || !errors.Is(forbidden, accountapp.ErrForbidden) {
			cleanup()
			t.Fatal("等待锁后必须复核当前归属")
		}
		cleanup()
	}
}
