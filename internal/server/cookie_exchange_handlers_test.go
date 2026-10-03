package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"xianyu-go/internal/xianyu/cookierefresh"
)

// TestCookieExchangeContracts 用 t 验证真实 Router 的两条成功契约、授权边界、重复提交及秘密隔离。
func TestCookieExchangeContracts(t *testing.T) {
	// srv、store、cleanup 是隔离 SQLite 的完整 Server 与资源释放责任。
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	// ctx 是本地夹具的数据库上下文。
	ctx := context.Background()
	// snapshot 保存虚构 Cookie，禁止在失败输出中打印。
	snapshot := []cookierefresh.BrowserCookie{{Name: "sid", Value: "fixture-secret", Domain: ".goofish.com", Path: "/", HTTPOnly: true, Secure: true}}
	if store.Cookies.UpdateRenewalCookie(ctx, "acc1", "sid=fixture-secret", cookierefresh.MetadataWithSnapshot(`{"device":"fixture"}`, snapshot), 123) != nil {
		t.Fatal("写入夹具失败")
	}
	// 停用账号仍可交换当前快照，但请求不得启用或启动其运行实例。
	if store.Cookies.SetStatus(ctx, "acc1", false) != nil {
		t.Fatal("停用账号夹具失败")
	}
	// handler 是真实完整路由；session 是当前账号所有者的认证 Cookie。
	handler := srv.Router()
	// session 仅作为 Helper 认证，不混入平台 Jar。
	session := loginHelper(t, handler)
	// call 用 method、path、body、authenticated 构造请求并校验对应 OpenAPI 响应。
	call := func(method, path, body string, authenticated bool) *httptest.ResponseRecorder {
		// request 是当前真实 transport 输入。
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if authenticated {
			request.AddCookie(session)
		}
		// recorder 捕获响应，失败不打印包含秘密的正文。
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assertOpenAPIResponse(t, request, recorder)
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("交换响应缺少 no-store")
		}
		if recorder.Code == 200 {
			assertOpenAPISuccessResponse(t, request, recorder)
		}
		return recorder
	}
	// base 是单账号交换路径。
	base := "/api/v1/integrations/accounts/acc1/"
	if call("GET", base+"cookie-snapshot", "", false).Code != 401 {
		t.Fatal("未认证应拒绝")
	}
	if call("POST", base+"cookie-updates", `{}`, false).Code != 401 {
		t.Fatal("未认证提交应拒绝")
	}
	// exported 是授权快照响应，不在错误日志中输出。
	exported := call("GET", base+"cookie-snapshot", "", true)
	if exported.Code != 200 {
		t.Fatal("快照导出失败")
	}
	// data 保存解码的专用 DTO，只用于检查契约属性。
	var data exchangeSnapshotDTO
	if json.Unmarshal(exported.Body.Bytes(), &data) != nil || len(data.Cookies) != 1 || !data.Cookies[0].HTTPOnly {
		t.Fatal("授权快照属性丢失")
	}
	// request 是 Reader 相同字段构造的增量，接收时间采用 UTC 毫秒。
	request := exchangeUpdatesDTO{Version: data.Version, Responses: []exchangeBatchDTO{{URL: "https://h5api.m.goofish.com/h5/example/1.0/", ReceivedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), SetCookies: []string{"sid=changed; Domain=.goofish.com; Path=/; HttpOnly; Secure"}}}}
	// encoded 是虚构增量序列化输入。
	encoded, _ := json.Marshal(request)
	// committed 是首次成功；未启动账号时同步状态必须为 not_running。
	committed := call("POST", base+"cookie-updates", string(encoded), true)
	if committed.Code != 200 {
		t.Fatal("增量提交失败")
	}
	// result 是不含秘密的提交 DTO。
	var result exchangeResultDTO
	if json.Unmarshal(committed.Body.Bytes(), &result) != nil || !result.Changed || result.SyncStatus != "not_running" || result.Version == data.Version {
		t.Fatal("提交状态不正确")
	}
	if store.Cookies.GetStatus(ctx, "acc1") {
		t.Fatal("Cookie 交换不得启用停用账号")
	}
	if call("POST", base+"cookie-updates", string(encoded), true).Code != 409 {
		t.Fatal("重复提交应版本冲突")
	}
	request.Version = result.Version
	request.Responses[0].SetCookies = []string{"invalid-cookie-header"}
	encoded, _ = json.Marshal(request)
	// unchanged 是浏览器规则拒绝头部后的成功无变化结果。
	unchanged := call("POST", base+"cookie-updates", string(encoded), true)
	if unchanged.Code != 200 || json.Unmarshal(unchanged.Body.Bytes(), &result) != nil || result.Changed || result.SyncStatus != "not_needed" {
		t.Fatal("拒绝头部应成功且不改变凭证")
	}
	if call("GET", "/api/v1/integrations/accounts/missing/cookie-snapshot", "", true).Code != 404 {
		t.Fatal("缺失账号应 404")
	}
	// created、userErr 是其他用户夹具创建结果。
	if created, userErr := store.Users.Create(ctx, "foreign", "foreign@example.test", "fixture-password"); userErr != nil || !created {
		t.Fatal("创建用户失败")
	}
	if store.Cookies.Save(ctx, "foreign", "secret", 2) != nil {
		t.Fatal("创建其他用户夹具失败")
	}
	if call("GET", "/api/v1/integrations/accounts/foreign/cookie-snapshot", "", true).Code != 403 {
		t.Fatal("管理员也不能导出其他用户账号")
	}
	if call("POST", "/api/v1/integrations/accounts/foreign/cookie-updates", string(encoded), true).Code != 403 {
		t.Fatal("跨用户提交应拒绝")
	}
	if store.Cookies.UpdateRenewalCookie(ctx, "acc1", "", cookierefresh.MetadataWithSnapshot(`{}`, nil), 123) != nil {
		t.Fatal("写入空快照失败")
	}
	if call("GET", base+"cookie-snapshot", "", true).Code != 409 {
		t.Fatal("空快照应 409")
	}
	if call("POST", base+"cookie-updates", string(encoded), true).Code != 409 {
		t.Fatal("无快照不能提交")
	}
	// ordinary 是普通账号详情；专用导出边界不扩大其秘密字段。
	ordinary := httptest.NewRequest("GET", "/api/v1/accounts/details", nil)
	ordinary.AddCookie(session)
	// ordinaryResponse 捕获普通账号响应，检查秘密隔离。
	ordinaryResponse := httptest.NewRecorder()
	handler.ServeHTTP(ordinaryResponse, ordinary)
	if strings.Contains(ordinaryResponse.Body.String(), "fixture-secret") || strings.Contains(ordinaryResponse.Body.String(), `"cookies"`) {
		t.Fatal("普通账号接口泄露秘密")
	}
}

// TestCookieExchangeInputBoundaries 用 t 验证来源、时间顺序、结构与字节上限，不执行外部请求。
func TestCookieExchangeInputBoundaries(t *testing.T) {
	// now 是稳定的本地校验时刻。
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	// valid 是合法协议最小请求。
	valid := exchangeUpdatesDTO{Version: "v1:" + strings.Repeat("a", 64), Responses: []exchangeBatchDTO{{URL: "https://www.goofish.com/im", ReceivedAt: now.Format("2006-01-02T15:04:05.000Z"), SetCookies: []string{"sid=v"}}}}
	// testCase 描述单项无效来源或格式。
	for _, testCase := range []string{"http://www.goofish.com/im", "https://evil.com/im", "https://www.goofish.com.evil/im", "https://u@www.goofish.com/im", "https://www.goofish.com:444/im", "https://www.goofish.com/im?q=signature", "https://www.goofish.com/im#fragment", "https://www.goofish.com./im", "https://www.goofish.com:/im"} {
		// request 是独立响应切片，防止污染合法夹具。
		request := exchangeUpdatesDTO{Version: valid.Version, Responses: []exchangeBatchDTO{valid.Responses[0]}}
		request.Responses[0].URL = testCase
		// ok 表示非法来源或时间是否被误接受。
		if _, ok := validateExchangeUpdates(request, now); ok {
			t.Fatal("非法来源未被拒绝")
		} // ok 是来源校验通过标记。
	}
	// offset 是窗口外或合法边界的秒数。
	for _, offset := range []int{-601, 31} {
		// request 是时间窗口外输入。
		request := exchangeUpdatesDTO{Version: valid.Version, Responses: []exchangeBatchDTO{valid.Responses[0]}}
		request.Responses[0].ReceivedAt = now.Add(time.Duration(offset) * time.Second).Format("2006-01-02T15:04:05.000Z")
		// ok 表示非法来源或时间是否被误接受。
		if _, ok := validateExchangeUpdates(request, now); ok {
			t.Fatal("过期回放未被拒绝")
		} // ok 是时间校验标记。
	}
	// ok 表示合法协议输入是否通过。
	if _, ok := validateExchangeUpdates(valid, now); !ok {
		t.Fatal("合法输入被拒绝")
	} // ok 表示协议最小输入有效。
	// srv、_、cleanup 是真实路由及资源释放责任。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// handler、session 是真实路由与授权会话。
	handler := srv.Router()
	// session 用于结构验证，保持与平台 Cookie 分离。
	session := loginHelper(t, handler)
	// body 是各种结构损坏或体积超限输入。
	for _, body := range []string{`{`, `{} {}`, `{}`, `{"unknown":"secret"}`, strings.Repeat(" ", exchangeBodyLimit+1)} {
		// request 是未经 transport 归一化的输入。
		request := httptest.NewRequest("POST", "/api/v1/integrations/accounts/acc1/cookie-updates", strings.NewReader(body))
		request.AddCookie(session)
		// recorder 捕获非敏感错误响应。
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		// expected 以字节上限区分 400 与 413。
		expected := 400
		if len(body) > exchangeBodyLimit {
			expected = 413
		}
		if recorder.Code != expected {
			t.Fatalf("结构错误状态应 %d，实际 %d", expected, recorder.Code)
		}
		assertOpenAPIResponse(t, request, recorder)
	}
}

// TestCookieExchangeAdditionalBoundaries 用 t 覆盖批次、头部、版本、时间精度和接收顺序的边界。
func TestCookieExchangeAdditionalBoundaries(t *testing.T) {
	// now 是固定的 UTC 校验时刻。
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	// batch 是合法单跳。
	batch := exchangeBatchDTO{URL: "https://seller.goofish.com:443/im", ReceivedAt: now.Format("2006-01-02T15:04:05.000Z"), SetCookies: []string{"sid=v"}}
	// cases 是每种结构或边界错误的独立输入。
	cases := []exchangeUpdatesDTO{
		{Version: "bad-version", Responses: []exchangeBatchDTO{batch}},
		{Version: "v1:" + strings.Repeat("a", 64)},
		{Version: "v1:" + strings.Repeat("a", 64), Responses: make([]exchangeBatchDTO, 33)},
	}
	// headers 是空数组、空头、过量头、过长头及换行注入。
	for _, headers := range [][]string{nil, {""}, make([]string, 129), {strings.Repeat("a", 8193)}, {"sid=v\r\nAuthorization: secret"}} {
		// copyBatch 保持基准请求不被污染。
		copyBatch := batch
		copyBatch.SetCookies = headers
		cases = append(cases, exchangeUpdatesDTO{Version: "v1:" + strings.Repeat("a", 64), Responses: []exchangeBatchDTO{copyBatch}})
	}
	// timestamp 是格式错误、非 UTC、非毫秒或时间倒序输入。
	for _, timestamp := range []string{"bad", "2026-10-03T08:00:00Z", "2026-10-03T08:00:00.000+00:00", "2026-10-03T08:00:00.0000Z", "2026-10-03T07:59:59.000Z"} {
		// copyBatch 是第二跳，必须不早于首跳。
		copyBatch := batch
		copyBatch.ReceivedAt = timestamp
		cases = append(cases, exchangeUpdatesDTO{Version: "v1:" + strings.Repeat("a", 64), Responses: []exchangeBatchDTO{batch, copyBatch}})
	}
	// request 是当前必须拒绝的输入。
	for _, request := range cases {
		// valid 是校验结果，失败不输出头部值。
		_, valid := validateExchangeUpdates(request, now)
		if valid {
			t.Fatal("边界损坏输入应拒绝")
		}
	}
	// host 是固定白名单中的合法 HTTPS 主机。
	for _, host := range []string{"www.goofish.com", "passport.goofish.com", "seller.goofish.com", "h5api.m.goofish.com"} {
		// copyBatch 分别覆盖默认端口和原时刻窗口边界。
		copyBatch := batch
		copyBatch.URL = "https://" + host + "/h5/example/"
		copyBatch.ReceivedAt = now.Add(-10 * time.Minute).Format("2006-01-02T15:04:05.000Z")
		// valid 表示允许主机和时间下限未被误拒绝。
		_, valid := validateExchangeUpdates(exchangeUpdatesDTO{Version: "v1:" + strings.Repeat("a", 64), Responses: []exchangeBatchDTO{copyBatch}}, now)
		if !valid {
			t.Fatal("合法来源及时间下限应接受")
		}
		copyBatch.ReceivedAt = now.Add(30 * time.Second).Format("2006-01-02T15:04:05.000Z")
		// accepted 表示允许未来三十秒的时间上限。
		if _, accepted := validateExchangeUpdates(exchangeUpdatesDTO{Version: "v1:" + strings.Repeat("a", 64), Responses: []exchangeBatchDTO{copyBatch}}, now); !accepted {
			t.Fatal("时间上限应接受")
		} // accepted 表示允许未来三十秒的边界。
	}
}

// TestCookieExchangeReaderSimulation 用 t 按 Reader 文档以独立 HTTP 客户端模拟登录、获取和增量提交，无真实平台调用。
func TestCookieExchangeReaderSimulation(t *testing.T) {
	// srv、store、cleanup 是隔离完整 Helper 与数据库。
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	if store.Cookies.UpdateRenewalCookie(context.Background(), "acc1", "sid=fixture", cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{{Name: "sid", Value: "fixture", Domain: ".goofish.com", Path: "/"}}), 1) != nil {
		t.Fatal("初始化失败")
	}
	// host 是仅监听本地的 Helper 模拟服务，由本测试关闭。
	host := httptest.NewServer(srv.Router())
	defer host.Close()
	// helperJar 保存 Helper 会话，platformJar 保存平台临时会话，二者不能共享。
	helperJar, _ := cookiejar.New(nil)
	// platformJar 的独立性模拟 Reader 不向闲鱼转发 Helper 凭证。
	platformJar, _ := cookiejar.New(nil)
	// helperClient 是仅访问本地 Helper 的消费者，响应字段按原始协议读取。
	helperClient := &http.Client{Jar: helperJar, Timeout: 3 * time.Second}
	// login、loginErr 是文档指定的 session/login 响应。
	login, loginErr := helperClient.Post(host.URL+"/api/v1/session/login", "application/json", strings.NewReader(`{"username":"admin","password":"pw"}`))
	if loginErr != nil {
		t.Fatal("模拟 Reader 登录失败")
	}
	login.Body.Close()
	if login.StatusCode != 200 {
		t.Fatal("模拟 Reader 登录状态错误")
	}
	// helperURL 用于检查认证容器内是否已有会话。
	helperURL, _ := url.Parse(host.URL)
	// platformURL 是独立平台容器的作用域，不请求该地址。
	platformURL, _ := url.Parse("https://www.goofish.com/im")
	if len(helperJar.Cookies(helperURL)) == 0 || len(platformJar.Cookies(platformURL)) != 0 {
		t.Fatal("Helper 与平台会话容器隔离失败")
	}
	// snapshot、getErr 是 Reader 获取的真实 HTTP 快照响应。
	snapshot, getErr := helperClient.Get(host.URL + "/api/v1/integrations/accounts/acc1/cookie-snapshot")
	if getErr != nil {
		t.Fatal("模拟 Reader 获取失败")
	}
	defer snapshot.Body.Close()
	// exported 是消费者按字段名解码的短生命周期模型，不含数据库类型。
	var exported exchangeSnapshotDTO
	if snapshot.StatusCode != 200 || json.NewDecoder(snapshot.Body).Decode(&exported) != nil {
		t.Fatal("模拟 Reader 快照契约不匹配")
	}
	// payload 按 Reader 文档逐条保留 Set-Cookie，并透传版本，不自行计算。
	payload := fmt.Sprintf(`{"credential_version":%q,"responses":[{"response_url":"https://www.goofish.com/im","received_at":%q,"set_cookies":["sid=reader; Domain=.goofish.com; Path=/; HttpOnly"]}]}`, exported.Version, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	// update、updateErr 是 Reader 提交响应，完全不向平台发送 Helper 会话。
	update, updateErr := helperClient.Post(host.URL+"/api/v1/integrations/accounts/acc1/cookie-updates", "application/json", strings.NewReader(payload))
	if updateErr != nil {
		t.Fatal("模拟 Reader 提交失败")
	}
	defer update.Body.Close()
	// result 只消费版本、变化与同步状态，不恢复旧增量。
	var result exchangeResultDTO
	if update.StatusCode != 200 || json.NewDecoder(update.Body).Decode(&result) != nil || !result.Changed || result.Version == exported.Version {
		t.Fatal("模拟 Reader 提交契约不匹配")
	}
	if len(platformJar.Cookies(platformURL)) != 0 {
		t.Fatal("管理会话不得流入平台容器")
	}
}

// TestCookieExchangeTransportFailures 用 t 验证读取故障与存储故障的 HTTP 状态，错误正文不得含 Cookie 或底层秘密。
func TestCookieExchangeTransportFailures(t *testing.T) {
	// srv、store、cleanup 是隔离的 transport 夹具。
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	// handler 是真实路由，session 是 Helper 会话。
	handler := srv.Router()
	// session 只用于认证此错误路径。
	session := loginHelper(t, handler)
	// readFailure 模拟 HTTP 请求体读取失败，底层故障字符串含虚构秘密。
	readFailure := httptest.NewRequest("POST", "/api/v1/integrations/accounts/acc1/cookie-updates", iotest.ErrReader(fmt.Errorf("fixture-sensitive")))
	readFailure.AddCookie(session)
	// readResult 捕获读取失败的统一错误。
	readResult := httptest.NewRecorder()
	handler.ServeHTTP(readResult, readFailure)
	if readResult.Code != 400 || strings.Contains(readResult.Body.String(), "fixture-sensitive") {
		t.Fatal("读取失败应脱敏 400")
	}
	assertOpenAPIResponse(t, readFailure, readResult)
	// ctx 是本地数据库调用上下文。
	ctx := context.Background()
	if store.Cookies.UpdateRenewalCookie(ctx, "acc1", "sid=fixture", cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{{Name: "sid", Value: "fixture", Domain: ".goofish.com", Path: "/"}}), 1) != nil {
		t.Fatal("初始化失败")
	}
	// snapshotReq 是取得旧版本的授权请求。
	snapshotReq := httptest.NewRequest("GET", "/api/v1/integrations/accounts/acc1/cookie-snapshot", nil)
	snapshotReq.AddCookie(session)
	// snapshotResult 捕获仅授权可见的快照。
	snapshotResult := httptest.NewRecorder()
	handler.ServeHTTP(snapshotResult, snapshotReq)
	// snapshot 保存当前状态指纹。
	var snapshot exchangeSnapshotDTO
	if snapshotResult.Code != 200 || json.Unmarshal(snapshotResult.Body.Bytes(), &snapshot) != nil {
		t.Fatal("导出失败")
	}
	// triggerErr 注入保留读取能力的持久化故障。
	_, triggerErr := store.DB.ExecContext(ctx, `CREATE TRIGGER exchange_fail BEFORE UPDATE ON cookies BEGIN SELECT RAISE(ABORT, 'fixture-sensitive'); END`)
	if triggerErr != nil {
		t.Fatal("故障夹具创建失败")
	}
	// body 是可能泄露虚构秘密的增量，日志和错误不可引用。
	body := fmt.Sprintf(`{"credential_version":%q,"responses":[{"response_url":"https://www.goofish.com/im","received_at":%q,"set_cookies":["sid=fixture-sensitive; Domain=.goofish.com; Path=/"]}]}`, snapshot.Version, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	// updateReq 是合法协议但持久化失败的提交。
	updateReq := httptest.NewRequest("POST", "/api/v1/integrations/accounts/acc1/cookie-updates", strings.NewReader(body))
	updateReq.AddCookie(session)
	// updateResult 是非敏感错误响应。
	updateResult := httptest.NewRecorder()
	handler.ServeHTTP(updateResult, updateReq)
	if updateResult.Code != 500 || strings.Contains(updateResult.Body.String(), "fixture-sensitive") {
		t.Fatal("存储失败应脱敏 500")
	}
	assertOpenAPIResponse(t, updateReq, updateResult)
}
