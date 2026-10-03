package automation

import "encoding/json"

// extractOrderIDFromContent 从 contentJSON 卡片的订单跳转链接返回真实订单号；解码失败或无匹配链接时返回空串，兼容 intent.page.jumpUrl。
func extractOrderIDFromContent(contentJSON string) string {
	// c 保存平台卡片结构；无法解码时不能从普通文案猜测订单号。
	var c map[string]any
	if json.Unmarshal([]byte(contentJSON), &c) != nil {
		return ""
	}
	// path 仅覆盖承载订单详情链接的卡片字段，避免把业务键事件码误作订单号。
	for _, path := range [][]string{
		{"dxCard", "item", "main", "exContent", "button", "targetUrl"},
		{"dxCard", "item", "main", "exContent", "button", "intent", "page", "jumpUrl"},
		{"dxCard", "item", "main", "targetUrl"},
		{"dynamicOperation", "changeContent", "dxCard", "item", "main", "exContent", "button", "targetUrl"},
	} {
		if // id 是当前卡片跳转链接匹配到的订单号，非空才作为交易事实返回。
		id := matchOrderID(nestedString(c, path...)); id != "" {
			return id
		}
	}
	return ""
}
