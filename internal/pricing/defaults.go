package pricing

// Defaults are the vendors' published list prices as read on 2026-10-23
// (standard tier, per 1M tokens, no batch/flex/priority discounts). Rows with
// a Note were not confirmed on the vendor's own page: check them.
func Defaults() []Entry {
	const (
		usd = "USD"
		cny = "CNY"
		ds  = "DeepSeek: 高峰（北京时间工作日 9-12、14-18，法定节假日除外）为这里价格的 2 倍"
	)
	return []Entry{
		// DeepSeek (CNY; rates are off-peak, peak is 2x). https://api-docs.deepseek.com/zh-cn/quick_start/pricing
		{Model: "deepseek-v4.1-flash", Currency: cny, Input: 1, CachedInput: 0.02, Output: 4, PeakMultiplier: 2, Note: ds},
		{Model: "deepseek-v4-flash", Currency: cny, Input: 1, CachedInput: 0.02, Output: 4, PeakMultiplier: 2, Note: "已下线，按 V4.1 Flash 计费。" + ds},
		{Model: "deepseek-v4-flash-vision-exp", Currency: cny, Input: 1, CachedInput: 0.02, Output: 4, PeakMultiplier: 2, Note: "已下线，按 V4.1 Flash 计费。" + ds},
		{Model: "deepseek-v4-pro", Currency: cny, Input: 4.5, CachedInput: 0.15, Output: 13.5, PeakMultiplier: 2, Note: ds},

		// Anthropic. https://platform.claude.com/docs/en/about-claude/pricing
		{Model: "claude-fable-5", Currency: usd, Input: 10, CachedInput: 1, Output: 50},
		{Model: "claude-opus-5", Currency: usd, Input: 5, CachedInput: 0.5, Output: 25},
		{Model: "claude-opus-4.8", Currency: usd, Input: 5, CachedInput: 0.5, Output: 25},
		{Model: "claude-sonnet-5", Currency: usd, Input: 2, CachedInput: 0.2, Output: 10},
		{Model: "claude-sonnet-5.5", Currency: usd, Input: 2, CachedInput: 0.1, Output: 10},
		{Model: "claude-haiku-5.5", Currency: usd, Input: 0.1, CachedInput: 0.01, Output: 0.5,
			LongThreshold: 100001, LongInput: 0.5, LongCachedInput: 0.05, LongOutput: 2.5, Note: "提示超过 100K token 整个请求按长上下文价"},

		// OpenAI. https://developers.openai.com/api/docs/pricing
		{Model: "gpt-5.6-luna", Currency: usd, Input: 0.2, CachedInput: 0.02, Output: 1.2, LongThreshold: 272001, LongInput: 0.4, LongCachedInput: 0.04, LongOutput: 1.8},
		{Model: "gpt-5.6-sol", Currency: usd, Input: 4, CachedInput: 0.4, Output: 20, LongThreshold: 272001, LongInput: 8, LongCachedInput: 0.8, LongOutput: 30,
			Note: "促销价，至少到 2026-11-21；原价 5 / 30"},
		{Model: "gpt-5.6-terra", Currency: usd, Input: 2, CachedInput: 0.2, Output: 12, LongThreshold: 272001, LongInput: 4, LongCachedInput: 0.4, LongOutput: 18},
		{Model: "gpt-6-luna", Currency: usd, Input: 0.1, CachedInput: 0.01, Output: 0.5, LongThreshold: 272001, LongInput: 0.2, LongCachedInput: 0.02, LongOutput: 0.75},
		{Model: "gpt-6-sol", Currency: usd, Input: 2, CachedInput: 0.2, Output: 10, LongThreshold: 272001, LongInput: 4, LongCachedInput: 0.4, LongOutput: 15},
		{Model: "gpt-6.1-sol", Currency: usd, Input: 2, CachedInput: 0.1, Output: 10, LongThreshold: 272001, LongInput: 4, LongCachedInput: 0.2, LongOutput: 15},

		// xAI. https://docs.x.ai/docs/models
		{Model: "grok-4.5", Currency: usd, Input: 2, CachedInput: 0.3, Output: 6, LongThreshold: 200000, LongInput: 4, LongCachedInput: 0.6, LongOutput: 12},
		{Model: "grok-4.6", Currency: usd, Input: 2, CachedInput: 0.5, Output: 6, LongThreshold: 200000, LongInput: 4, LongCachedInput: 1, LongOutput: 12},
		{Model: "grok-4.7", Currency: usd, Input: 2, CachedInput: 0.5, Output: 6, LongThreshold: 200000, LongInput: 4, LongCachedInput: 1, LongOutput: 12},

		// Google. https://ai.google.dev/gemini-api/docs/pricing
		{Model: "gemini-3.7-flash", Currency: usd, Input: 0.75, CachedInput: 0.075, Output: 3.75,
			Note: "未在 Google 官方价格页找到，数值来自第三方聚合站，请核对"},
		{Model: "gemini-3.8-flash", Currency: usd, Input: 0.75, CachedInput: 0.075, Output: 3.75, Note: "促销价至 2026-12-31，2027-01-01 起翻倍"},
		{Model: "gemini-3.8-flash", Currency: usd, Input: 1.5, CachedInput: 0.15, Output: 7.5, From: "2027-01-01", Note: "促销结束后的价格"},
		{Model: "gemini-4-argon", Currency: usd, Input: 2, CachedInput: 0.1, Output: 10, Note: "促销价至 2026-11-30，12-01 起翻倍"},
		{Model: "gemini-4-argon", Currency: usd, Input: 4, CachedInput: 0.2, Output: 20, From: "2026-12-01", Note: "促销结束后的价格"},

		// Meta. https://dev.meta.ai/docs/pricing-rate-limits
		{Model: "muse-spark-1.3", Currency: usd, Input: 1.25, CachedInput: 0.15, Output: 4.25},
		{Model: "muse-wonder", Currency: usd, Input: 4, CachedInput: 0.2, Output: 20, Note: "推理 token 按输出计费"},

		// Tencent. https://cloud.tencent.com/document/product/1823/130055
		{Model: "hy4-preview", Currency: cny, Input: 6, CachedInput: 0.3, Output: 18},

		// Z.ai. https://docs.z.ai/guides/overview/pricing
		{Model: "glm-5.2", Currency: usd, Input: 1.4, CachedInput: 0.26, Output: 4.4},
		{Model: "glm-5.3-flash", Currency: usd, Input: 0.15, CachedInput: 0.03, Output: 0.5},
		{Model: "glm-5.3-flashx", Currency: usd, Input: 0.37, CachedInput: 0.075, Output: 1.25},

		// MiniMax. https://platform.minimax.io/docs/guides/pricing-paygo
		{Model: "minimax-m3.1-flash", Currency: usd, Input: 0.15, CachedInput: 0.015, Output: 0.6},

		// Alibaba (CNY, Beijing region). https://help.aliyun.com/en/model-studio/qwen3-7-max
		{Model: "qwen3.7-max", Currency: cny, Input: 12, CachedInput: 2.4, Output: 36},

		// Moonshot and Xiaomi: official pages showed no table; figures are third-party USD.
		{Model: "kimi-k2.7-code", Currency: usd, Input: 0.95, CachedInput: 0.19, Output: 4, Note: "官方价格页未渲染，数值来自第三方，请核对"},
		{Model: "kimi-k3", Currency: usd, Input: 3, CachedInput: 0.3, Output: 15, Note: "数值来自第三方，请核对"},
		{Model: "mimo-v2.6-flash", Currency: usd, Input: 0.14, CachedInput: 0.14, Output: 0.28, Note: "第三方数值；未找到缓存价，暂按输入价计，请核对"},
		{Model: "mimo-v2.6-pro", Currency: usd, Input: 0.435, CachedInput: 0.0036, Output: 0.87, Note: "第三方数值，请核对"},
	}
}
