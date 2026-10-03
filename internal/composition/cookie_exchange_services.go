package composition

import (
	"log/slog"

	"xianyu-go/internal/adapter"
	accountapp "xianyu-go/internal/application/account"
)

// newCookieExchangeService 仅将 dependencies 的仓储与 runtime 同步能力集中接线，不改变原有用例。
func newCookieExchangeService(dependencies Dependencies, runtime *accountapp.RuntimeService) *accountapp.CookieExchangeService {
	return accountapp.NewCookieExchangeService(dependencies.AccountDependencies.NewCookieExchangeRepository(dependencies.Logger), adapter.AccountRunningLookup(dependencies.Manager), runtime.UpdateCookie, cookieExchangeReporter(dependencies.Logger))
}

// cookieExchangeReporter 将可选 logger 映射为用例的固定阶段回调，禁止接收底层错误对象或 Cookie。
func cookieExchangeReporter(logger *slog.Logger) func(string) {
	// message 是交换用例预置的非敏感阶段，服务只传固定文本。
	return func(message string) {
		if logger != nil {
			logger.Warn(message)
		}
	}
}
