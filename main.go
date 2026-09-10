package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed admin.html
var adminHTML string

// srv 是网关 HTTP 服务句柄，用于重启前优雅关闭并释放监听端口
var srv *http.Server

// dayUsage 单日 token 消耗
type dayUsage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

// usageTracker 按自然日累计 token 消耗，可持久化到文件
type usageTracker struct {
	mu      sync.Mutex
	days    map[string]*dayUsage // key: "2006-01-02"
	path    string
	lastDay string // 上次持久化时的日期
}

func newUsageTracker(path string) *usageTracker {
	t := &usageTracker{days: map[string]*dayUsage{}, path: path}
	t.load()
	return t
}

func (t *usageTracker) load() {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &t.days)
}

func (t *usageTracker) save() {
	if t.path == "" {
		return
	}
	data, err := json.Marshal(t.days)
	if err != nil {
		return
	}
	_ = os.WriteFile(t.path, data, 0644)
}

// add 累加当天 token
func (t *usageTracker) add(prompt, completion int64) {
	if prompt <= 0 && completion <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := time.Now().Format("2006-01-02")
	d := t.days[key]
	if d == nil {
		d = &dayUsage{}
		t.days[key] = d
	}
	d.Prompt += prompt
	d.Completion += completion
	t.save()
}

// totals 返回 [今日, 本月, 今年(含今日), 累计] 的 token 消耗
func (t *usageTracker) totals() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	month := now.Format("2006-01")
	year := now.Format("2006")

	sum := func(from string) (p, c int64) {
		for k, d := range t.days {
			if k >= from && k <= today {
				p += d.Prompt
				c += d.Completion
			}
		}
		return
	}
	tp, tc := sum(today)
	yp, yc := sum(year)
	mp, mc := sum(month)
	// 自平台成立以来累计
	var allp, allc int64
	for _, d := range t.days {
		allp += d.Prompt
		allc += d.Completion
	}
	return map[string]interface{}{
		"today":  map[string]interface{}{"prompt": tp, "completion": tc, "total": tp + tc},
		"year":   map[string]interface{}{"prompt": yp, "completion": yc, "total": yp + yc},
		"month":  map[string]interface{}{"prompt": mp, "completion": mc, "total": mp + mc},
		"total":  map[string]interface{}{"prompt": allp, "completion": allc, "total": allp + allc},
		"daily":  t.dailySince(now.AddDate(0, 0, -29)), // 近30天明细，供图表
	}
}

// dailySince 返回 from 起每天（含今天）的消耗，按日期升序
func (t *usageTracker) dailySince(from time.Time) []map[string]interface{} {
	out := []map[string]interface{}{}
	for d := from; !d.After(time.Now()); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		du := t.days[key]
		p, c := int64(0), int64(0)
		if du != nil {
			p, c = du.Prompt, du.Completion
		}
		out = append(out, map[string]interface{}{
			"date":       key,
			"prompt":     p,
			"completion": c,
			"total":      p + c,
		})
	}
	return out
}

// computeSaved 按日计算各时段省钱金额：有模型维度数据的日子用精确值，无则用 fallback 费率估算。
func (t *usageTracker) computeSaved(dailySaved map[string]float64, fallbackIn, fallbackOut float64) (todaySaved, monthSaved, yearSaved, totalSaved float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	month := now.Format("2006-01")
	year := now.Format("2006")
	savedForDay := func(date string, p, c int64) float64 {
		if v, ok := dailySaved[date]; ok {
			return v
		}
		return float64(p)/1e6*fallbackIn + float64(c)/1e6*fallbackOut
	}
	for k, d := range t.days {
		s := savedForDay(k, d.Prompt, d.Completion)
		if k == today {
			todaySaved = s
		}
		if k >= year && k <= today {
			yearSaved += s
		}
		if k >= month && k <= today {
			monthSaved += s
		}
		totalSaved += s
	}
	return
}

// statsTracker 统计网关运行指标：今日请求数、成功率、429 次数、按分钟请求数（RPM）与按模型 token 用量，可持久化
type statsTracker struct {
	mu        sync.Mutex
	path      string
	days      map[string]*dayStats // key: "2006-01-02"
	minuteReq map[int64]int64      // key: Unix 分钟时间戳 -> 请求数（仅内存，供 RPM 曲线）
	lastDay   string
}

type dayStats struct {
	Requests int64                  `json:"requests"`
	Success  int64                  `json:"success"`
	Fail     int64                  `json:"fail"`    // 网络错误/超时/5xx
	Rate429  int64                  `json:"rate_429"` // 429 限流次数
	Models   map[string]*modelUsage `json:"models"`  // 按模型 token 用量
}

type modelUsage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

func newStatsTracker(path string) *statsTracker {
	s := &statsTracker{days: map[string]*dayStats{}, minuteReq: map[int64]int64{}, path: path}
	s.load()
	return s
}

func (s *statsTracker) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &s.days)
}

func (s *statsTracker) save() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.days)
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, data, 0644)
}

func (s *statsTracker) today() *dayStats {
	key := time.Now().Format("2006-01-02")
	d := s.days[key]
	if d == nil {
		d = &dayStats{Models: map[string]*modelUsage{}}
		s.days[key] = d
	}
	return d
}

// record 记录一次请求的结果（success/fail/rate_429），并计入 RPM 每分钟请求数
func (s *statsTracker) record(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.today()
	d.Requests++
	switch outcome {
	case "success":
		d.Success++
	case "rate_429":
		d.Rate429++
		d.Fail++
	default: // fail
		d.Fail++
	}
	// RPM：累计到当前分钟桶
	now := time.Now()
	mk := now.Unix() / 60
	s.minuteReq[mk]++
	// 清理超过 2 小时前的桶，避免内存无界增长
	if len(s.minuteReq) > 240 {
		cutoff := now.Add(-2 * time.Hour).Unix() / 60
		for k := range s.minuteReq {
			if k < cutoff {
				delete(s.minuteReq, k)
			}
		}
	}
	s.save()
}

// recordUsage 记录某模型一次成功的 token 消耗
func (s *statsTracker) recordUsage(model string, prompt, completion int64) {
	if prompt <= 0 && completion <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.today()
	mu := d.Models[model]
	if mu == nil {
		mu = &modelUsage{}
		d.Models[model] = mu
	}
	mu.Prompt += prompt
	mu.Completion += completion
	s.save()
}

// snapshot 返回概览页所需指标：今日健康度、RPM 曲线、按模型 token 拆分（含节省金额）
func (s *statsTracker) snapshot(prices map[string]ModelPrice) map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	d := s.days[today]
	req, ok, fail, r429 := int64(0), int64(0), int64(0), int64(0)
	if d != nil {
		req, ok, fail, r429 = d.Requests, d.Success, d.Fail, d.Rate429
	}
	rate := 0.0
	if req > 0 {
		rate = float64(ok) / float64(req) * 100
	}
	// RPM：最近 60 分钟
	rpm := make([]map[string]interface{}, 0, 60)
	curMin := now.Unix() / 60
	for i := 59; i >= 0; i-- {
		mk := curMin - int64(i)
		t := time.Unix(mk*60, 0)
		rpm = append(rpm, map[string]interface{}{
			"time":     t.Format("15:04"),
			"requests": s.minuteReq[mk],
		})
	}
	// 按模型 token 拆分（今日）+ 各模型节省金额
	models := map[string]interface{}{}
	todayCost := 0.0
	if d != nil {
		for m, mu := range d.Models {
			c := costOf(prices[m], mu.Prompt, mu.Completion)
			todayCost += c
			models[m] = map[string]interface{}{
				"prompt":     mu.Prompt,
				"completion": mu.Completion,
				"total":      mu.Prompt + mu.Completion,
				"cost":       c,
			}
		}
	}
	return map[string]interface{}{
		"today": map[string]interface{}{
			"requests":    req,
			"success":     ok,
			"fail":        fail,
			"rate_429":    r429,
			"success_rate": rate,
			"cost":        todayCost,
		},
		"rpm":    rpm,
		"models": models,
	}
}

// savedByPeriod 按每日实际模型分布计算精确省钱金额。
// 返回每日 saved（元）map，以及全期加权混合输入/输出单价（用于 stats.json 缺失日期的 fallback 估算）。
func (s *statsTracker) savedByPeriod(prices map[string]ModelPrice) (dailySaved map[string]float64, allTimeInPerM, allTimeOutPerM float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dailySaved = map[string]float64{}
	var allInCost, allOutCost float64
	var allP, allC int64
	for date, d := range s.days {
		var daySaved float64
		for m, mu := range d.Models {
			p := prices[m]
			c := costOf(p, mu.Prompt, mu.Completion)
			daySaved += c
			allInCost += float64(mu.Prompt) / 1e6 * p.Input
			allOutCost += float64(mu.Completion) / 1e6 * p.Output
			allP += mu.Prompt
			allC += mu.Completion
		}
		dailySaved[date] = daySaved
	}
	if allP > 0 {
		allTimeInPerM = allInCost * 1e6 / float64(allP)
	}
	if allC > 0 {
		allTimeOutPerM = allOutCost * 1e6 / float64(allC)
	}
	return
}

// Account 一个上游账号（一个 API Key 及其已开通的模型）
type Account struct {
	Alias     string   `json:"alias"`
	ApiKey    string   `json:"api_key"`
	Models    []string `json:"models"`
	CreatedAt string   `json:"created_at"` // 创建时间（RFC3339），可为空
}

// ModelPrice 某模型单价（元 / 百万 tokens），用于计算「已为你节省多少钱」
type ModelPrice struct {
	Input  float64 `json:"input"`  // 输入单价（元/百万tokens）
	Output float64 `json:"output"` // 输出单价（元/百万tokens）
}

// ModelQuota 某模型的免费额度限制（严格参考官网「每窗口时长内最多调用次数」）
type ModelQuota struct {
	WindowSeconds int `json:"window_seconds"` // 窗口时长（秒），官网免费额度为 5 小时 = 18000
	MaxCalls      int `json:"max_calls"`      // 窗口内最大调用次数
}

// Config 网关配置
type Config struct {
	Listen          string                 `json:"listen"`
	UpstreamBase    string                 `json:"upstream_base"`
	TimeoutSeconds  int                    `json:"timeout_seconds"`
	CooldownSeconds int                    `json:"cooldown_seconds"`
	Cooldown429     int                    `json:"cooldown_429_seconds"` // 429 限流冷却时长（更长，避免反复踩刚恢复的 Key）
	DisableStream   bool                   `json:"disable_stream"`
	MaxTokensLimit  int                    `json:"max_tokens_limit"` // 强制 max_tokens 上限（0=不限制），降低 TPM 消耗
	PriorityModels  []string               `json:"priority_models"`  // 优先模型：允许工具调用，且在列表中靠前
	ModelPrices     map[string]ModelPrice  `json:"model_prices"`     // 各模型单价（元/百万tokens），用于计算节省金额
	ModelQuotas     map[string]ModelQuota  `json:"model_quotas"`     // 各模型免费额度（官网「每5小时N次」），未配置则不限流
	Accounts        []Account              `json:"accounts"`
}

// costOf 计算 (prompt, completion) tokens 按某模型官网单价折算成的估算金额（元）
func costOf(p ModelPrice, prompt, completion int64) float64 {
	return float64(prompt)/1e6*p.Input + float64(completion)/1e6*p.Output
}

type accountState struct {
	failCount    int
	disableUntil time.Time
	callCount    int64  // 累计调用次数
	lastCall     time.Time // 最新调用时间
	activeCalls  int      // 当前进行中的调用数（>0 表示「使用中」）
	quotaStart   time.Time // 当前额度窗口起点（未启用限流时为零值）
	quotaCalls   int       // 当前额度窗口内已调用次数
	consec429    int       // 连续 429 次数，用于指数退避（成功后清零）
}

// Gateway 网关运行时
type Gateway struct {
	cfg          Config
	mu           sync.RWMutex
	states       map[string]*accountState // key: "账号下标:模型" -> 独立冷却状态
	modelIndex   map[string][]int         // model id -> 账号下标列表
	rrCursor     map[string]int           // model id -> 轮询游标
	client       *http.Client             // 非流式：带整体超时
	streamClient *http.Client             // 流式：仅响应头超时，避免长流被误杀
	usage        *usageTracker            // token 消耗统计（今日/7日/本月）
	stats        *statsTracker            // 运行指标：请求数/成功率/429/RPM/按模型token
	cooldownPath string                    // 冷却状态持久化文件路径（跨重启保留，避免重启后错误显示「可用」）
	imgHisPath   string                    // 生图记录持久化文件路径
	imgDir       string                    // 生图图片本地存储目录（下网上游临时URL，历史图片可长期查看）
	imgHis       []imageHistoryRecord      // 生图历史记录（倒序：最新的在最前）
	imgMu        sync.Mutex                // 生图历史记录互斥锁
}

func loadConfig(path string) (Config, error) {
	cfg := Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	applyDefaults(&cfg)
	return cfg, nil
}

// applyDefaults 填充配置默认值
func applyDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:18888"
	}
	if cfg.UpstreamBase == "" {
		cfg.UpstreamBase = "https://api.example.com/v1"
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 60
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = 300
	}
	if cfg.Cooldown429 <= 0 {
		cfg.Cooldown429 = 600
	}
	// 默认模型单价（元/百万tokens），按各模型官网公开价格：
	// deepseek-v4-flash：输入 1.5 元、输出 4.5 元（空闲时段标准价）
	// glm-5.2：输入 8 元、输出 28 元（智谱 bigmodel.cn 官方价）
	if cfg.ModelPrices == nil {
		cfg.ModelPrices = map[string]ModelPrice{}
	}
	if _, ok := cfg.ModelPrices["deepseek-v4-flash"]; !ok {
		cfg.ModelPrices["deepseek-v4-flash"] = ModelPrice{Input: 1.5, Output: 4.5}
	}
	if _, ok := cfg.ModelPrices["glm-5.2"]; !ok {
		cfg.ModelPrices["glm-5.2"] = ModelPrice{Input: 8, Output: 28}
	}
	// 模型免费额度限流（可选）。官网自 2026-08-28 起改为「积分制」
	// （60,000 积分滚动 5 小时 + 600,000 积分滚动周），不再按「每5小时N次」计。
	// 上游 /v1/models 的 pricing 均为 0（不返回各模型积分单价），无法精确按积分限流，
	// 故默认不启用（空 map = 不限流）。如需限流，可在配置中手动添加 model_quotas。
	if cfg.ModelQuotas == nil {
		cfg.ModelQuotas = map[string]ModelQuota{}
	}
	cfg.UpstreamBase = strings.TrimRight(cfg.UpstreamBase, "/")
}

func newTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
	}
}

func newGateway(cfg Config, usagePath string) *Gateway {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	transport := newTransport(timeout)
	g := &Gateway{
		cfg:          cfg,
		states:       map[string]*accountState{},
		modelIndex:   buildModelIndex(cfg.Accounts),
		rrCursor:     map[string]int{},
		client:       &http.Client{Transport: transport, Timeout: timeout},
		streamClient: &http.Client{Transport: transport},
		usage:        newUsageTracker(usagePath),
		stats:        newStatsTracker(filepath.Join(filepath.Dir(usagePath), "stats.json")),
		cooldownPath: filepath.Join(filepath.Dir(usagePath), "cooldown.json"),
	}
	g.loadCooldownState()
	return g
}

// buildModelIndex 根据账号列表构建 model id -> 账号下标 的索引
func buildModelIndex(accounts []Account) map[string][]int {
	idx := map[string][]int{}
	for i, acc := range accounts {
		for _, m := range acc.Models {
			if m = strings.TrimSpace(m); m != "" {
				idx[m] = append(idx[m], i)
			}
		}
	}
	return idx
}

// reload 用新配置热重载：重建模型索引、HTTP 客户端，保留冷却状态与轮询游标
func (g *Gateway) reload(newCfg Config) {
	timeout := time.Duration(newCfg.TimeoutSeconds) * time.Second
	transport := newTransport(timeout)
	g.mu.Lock()
	defer g.mu.Unlock()

	// 保留冷却状态：按 api_key 映射到新下标，避免「保存配置」时清空所有冷却
	oldKeyIdx := map[string]int{}
	for i, acc := range g.cfg.Accounts {
		oldKeyIdx[acc.ApiKey] = i
	}
	newStates := map[string]*accountState{}
	for i, acc := range newCfg.Accounts {
		oldIdx, ok := oldKeyIdx[acc.ApiKey]
		if !ok {
			continue
		}
		for _, m := range acc.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if st := g.states[stateKey(oldIdx, m)]; st != nil {
				newStates[stateKey(i, m)] = st
			}
		}
	}

	g.cfg = newCfg
	g.modelIndex = buildModelIndex(newCfg.Accounts)
	g.states = newStates
	g.rrCursor = map[string]int{}
	g.client = &http.Client{Transport: transport, Timeout: timeout}
	g.streamClient = &http.Client{Transport: transport}
	log.Printf("[网关] 配置已热重载：账号 %d 个，模型 %d 个，超时 %d 秒",
		len(newCfg.Accounts), len(modelSet(newCfg)), newCfg.TimeoutSeconds)
}

func stateKey(ai int, model string) string {
	return fmt.Sprintf("%d:%s", ai, model)
}

// stateFor 返回某个 (账号, 模型) 的冷却状态，调用方须已持有锁
func (g *Gateway) stateFor(ai int, model string) *accountState {
	key := stateKey(ai, model)
	st := g.states[key]
	if st == nil {
		st = &accountState{}
		g.states[key] = st
	}
	return st
}

// persistedCooldown 持久化到磁盘的冷却状态（跨重启保留）
type persistedCooldown struct {
	DisableUntil int64 `json:"disable_until"` // Unix 秒
	Consec429    int   `json:"consec429"`
	FailCount    int   `json:"fail_count"`
}

// accountKey 返回账号的稳定标识（api_key），用于跨重启/重载映射冷却状态
func (g *Gateway) accountKey(ai int) string {
	if ai >= 0 && ai < len(g.cfg.Accounts) {
		return g.cfg.Accounts[ai].ApiKey
	}
	return fmt.Sprintf("idx-%d", ai)
}

// saveCooldownState 把当前冷却状态写入磁盘，避免进程重启后账号错误显示「可用」。
// 调用方不得持有 g.mu 锁。
func (g *Gateway) saveCooldownState() {
	if g.cooldownPath == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	data := map[string]persistedCooldown{}
	for key, st := range g.states {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		var ai int
		if _, err := fmt.Sscanf(parts[0], "%d", &ai); err != nil {
			continue
		}
		if st.disableUntil.IsZero() && st.consec429 == 0 {
			continue
		}
		data[g.accountKey(ai)+"|"+parts[1]] = persistedCooldown{
			DisableUntil: st.disableUntil.Unix(),
			Consec429:    st.consec429,
			FailCount:    st.failCount,
		}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = os.WriteFile(g.cooldownPath, b, 0644)
}

// loadCooldownState 从磁盘恢复冷却状态（仅在启动时调用，单线程无锁竞争）
func (g *Gateway) loadCooldownState() {
	if g.cooldownPath == "" {
		return
	}
	raw, err := os.ReadFile(g.cooldownPath)
	if err != nil {
		return
	}
	var data map[string]persistedCooldown
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	keyIdx := map[string]int{}
	for i, acc := range g.cfg.Accounts {
		keyIdx[acc.ApiKey] = i
	}
	now := time.Now()
	for pkey, pc := range data {
		parts := strings.SplitN(pkey, "|", 2)
		if len(parts) != 2 {
			continue
		}
		ai, ok := keyIdx[parts[0]]
		if !ok {
			continue // 账号已被删除，忽略
		}
		until := time.Unix(pc.DisableUntil, 0)
		if now.After(until) && pc.Consec429 == 0 {
			continue // 已过期且无退避记录，无需恢复
		}
		st := g.stateFor(ai, parts[1])
		st.disableUntil = until
		st.consec429 = pc.Consec429
		st.failCount = pc.FailCount
	}
}

// nextAccount 针对指定模型，轮询选择下一个未冷却的账号
func (g *Gateway) nextAccount(model string) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	idxs := g.modelIndex[model]
	if len(idxs) == 0 {
		return -1, false
	}
	cursor := g.rrCursor[model]
	now := time.Now()
	for k := 0; k < len(idxs); k++ {
		ai := idxs[(cursor+k)%len(idxs)]
		if now.After(g.stateFor(ai, model).disableUntil) {
			g.rrCursor[model] = (cursor + k + 1) % len(idxs)
			return ai, true
		}
	}
	return -1, false
}

// cooldown 令某个 (账号, 模型) 进入冷却，避免反复踩到故障账号。
// seconds 为冷却时长；429 限流用更长的 Cooldown429，避免反复踩刚恢复的 Key。
func (g *Gateway) cooldown(ai int, model, reason string, seconds int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateFor(ai, model)
	st.failCount++
	now := time.Now()
	if now.After(st.disableUntil) {
		log.Printf("[网关] 账号[%s] 模型[%s] %s，冷却 %d 秒", g.cfg.Accounts[ai].Alias, model, reason, seconds)
	}
	st.disableUntil = now.Add(time.Duration(seconds) * time.Second)
}

// tryConsumeQuota 尝试消耗一次调用额度。若模型配置了免费额度（官网「每5小时N次」），
// 则按窗口计数；达到上限时返回 false 并将该账号冷却到窗口结束，
// 从根本上避免「后台看着可用、一用就立即 429」的问题。未配置限流的模型始终返回 true。
func (g *Gateway) tryConsumeQuota(ai int, model string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	q, ok := g.cfg.ModelQuotas[model]
	if !ok || q.WindowSeconds <= 0 || q.MaxCalls <= 0 {
		return true
	}
	st := g.stateFor(ai, model)
	now := time.Now()
	win := time.Duration(q.WindowSeconds) * time.Second
	if st.quotaStart.IsZero() || now.Sub(st.quotaStart) >= win {
		st.quotaStart = now
		st.quotaCalls = 0
	}
	if st.quotaCalls >= q.MaxCalls {
		st.disableUntil = st.quotaStart.Add(win)
		return false
	}
	st.quotaCalls++
	return true
}

// markCallStart 记录一次调用开始：调用次数 +1、刷新最新时间、活跃数 +1（用于「使用中」状态）
func (g *Gateway) markCallStart(ai int, model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateFor(ai, model)
	st.callCount++
	st.lastCall = time.Now()
	st.activeCalls++
}

// markCallEnd 调用结束（成功透传或异常切换），活跃数 -1
func (g *Gateway) markCallEnd(ai int, model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateFor(ai, model)
	if st.activeCalls > 0 {
		st.activeCalls--
	}
}

func modelSet(cfg Config) []string {
	set := map[string]bool{}
	for _, acc := range cfg.Accounts {
		for _, m := range acc.Models {
			if m = strings.TrimSpace(m); m != "" {
				set[m] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func extractModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return strings.TrimSpace(m.Model)
}

func extractStream(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Stream
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// autoModelID 是 Auto Mode 的特殊模型 ID：客户端请求该模型时，
// 网关按优先级自动选择一个当前可用的对话模型，某个模型不可用则自动切换下一个。
const autoModelID = "auto"

// isImageModel 判断模型是否为图像生成模型（走 /v1/images 接口，不参与对话 Auto 选择）
func isImageModel(model string) bool {
	switch model {
	case "u1-fast", "u1.5-lite":
		return true
	}
	return false
}

// replaceModelInBody 将请求体中的 model 字段替换为指定模型，返回新 body
func replaceModelInBody(body []byte, model string) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = model
	nb, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return nb
}

// pickAutoModelList 按 PriorityModels 顺序返回所有已配置账号的对话模型（图像模型除外）。
// Auto 模式下按此顺序逐个尝试，某个模型全部不可用则自动降级到下一个。
func (g *Gateway) pickAutoModelList() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []string
	for _, m := range g.cfg.PriorityModels {
		m = strings.TrimSpace(m)
		if m == "" || isImageModel(m) {
			continue
		}
		if len(g.modelIndex[m]) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// stripReasoning 对指定模型关闭思考模式：deepseek-v4-flash 默认开启思考，
// 会额外返回 reasoning_content，拖慢流式响应并干扰客户端解析。
// 官方通过 thinking.type 控制开关（enabled/disabled），这里强制关闭。
func stripReasoning(body []byte, model string) []byte {
	if !hasReasoning(model) {
		return body
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	// deepseek-v4-flash 的思考开关是 thinking.type，而非 enable_thinking。
	// 旧代码删除 enable_thinking / reasoning_effort 均无效，导致思考始终开启。
	m["thinking"] = map[string]interface{}{"type": "disabled"}
	nb, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return nb
}

// limitMaxTokens 将请求体中的 max_tokens 限制在指定上限内，避免单次生成
// 过多 token 快速耗尽 TPM（每分钟 token）额度。0 表示不限制。
func limitMaxTokens(body []byte, limit int) []byte {
	if limit <= 0 {
		return body
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if v, ok := m["max_tokens"].(float64); ok && v > 0 && v <= float64(limit) {
		return body
	}
	m["max_tokens"] = limit
	nb, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return nb
}

// ensureStreamUsage 对流式请求注入 stream_options.include_usage=true，
// 让 glm-5.2 等默认不返回 usage 的模型也在流式末尾回传 token 用量，
// 否则这些模型的 token 消耗无法统计。若客户端已显式设置则保留原值。
func ensureStreamUsage(body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	// 已存在 stream_options 则不覆盖
	if so, ok := m["stream_options"].(map[string]interface{}); ok {
		if _, exists := so["include_usage"]; exists {
			return body
		}
	}
	m["stream_options"] = map[string]interface{}{"include_usage": true}
	nb, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return nb
}

var reasoningModels = map[string]bool{
	"deepseek-v4-flash": true,
}

func hasReasoning(model string) bool {
	return reasoningModels[model]
}

// forceNoToolChoice 当客户端（如 Trae Agent）给每条消息都塞入 tools
// 但实际是普通对话时，强制 tool_choice=none，让模型直接文字回答，
// 避免模型误以为要调用工具、输出 tool_calls 导致客户端读不到 content。
// 优先模型（priority_models）保留工具调用能力，以便 Agent 真正干活。
func (g *Gateway) forceNoToolChoice(body []byte, model string) []byte {
	if g.isPriorityModel(model) {
		return body
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, hasTools := m["tools"]; !hasTools {
		return body
	}
	if v, ok := m["tool_choice"]; ok && v.(string) == "none" {
		return body
	}
	if _, alreadyAny := m["tool_choice"]; alreadyAny {
		return body
	}
	m["tool_choice"] = "none"
	nb, err := json.Marshal(m)
	if err != nil {
		return body
	}
	log.Printf("[网关] 模型[%s] 非优先模型，强制 tool_choice=none", model)
	return nb
}

// isPriorityModel 判断模型是否在优先列表中（优先模型允许工具调用）
func (g *Gateway) isPriorityModel(model string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, p := range g.cfg.PriorityModels {
		if strings.TrimSpace(p) == model {
			return true
		}
	}
	return false
}

// disableStreamInBody 将请求体中的 stream 置为 false，返回新 body
func disableStreamInBody(body []byte) ([]byte, bool) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if s, ok := m["stream"].(bool); !ok || !s {
		return body, false
	}
	m["stream"] = false
	nb, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return nb, true
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}
	g.forward(w, r, "/chat/completions", body)
}

func (g *Gateway) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}
	g.forward(w, r, "/images/generations", body)
}

// imageHistoryRecord 一条生图历史记录
type imageHistoryRecord struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Prompt  string   `json:"prompt"`
	Size    string   `json:"size"`
	Time    int64    `json:"time"`   // Unix 毫秒
	Local   []string `json:"local"`  // 本地图片访问路径，如 /admin/image-file/xxx.png
	Sources []string `json:"sources"` // 上游原始 URL（本地下载失败时保留备用）
}

// loadImageHistory 从磁盘读取生图历史
func (g *Gateway) loadImageHistory() {
	if g.imgHisPath == "" {
		return
	}
	raw, err := os.ReadFile(g.imgHisPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &g.imgHis)
}

// persistImageHistory 把生图历史写入磁盘
func (g *Gateway) persistImageHistory() {
	if g.imgHisPath == "" {
		return
	}
	b, err := json.Marshal(g.imgHis)
	if err != nil {
		return
	}
	_ = os.WriteFile(g.imgHisPath, b, 0644)
}

// saveImageRecord 保存一条生图记录（含本地下载），返回本地路径列表
func (g *Gateway) saveImageRecord(model, prompt, size string, urls []string) []string {
	g.imgMu.Lock()
	defer g.imgMu.Unlock()
	local := []string{}
	for _, u := range urls {
		if u == "" {
			continue
		}
		_, saved := g.downloadImage(u)
		if saved != "" {
			local = append(local, "/admin/image-file/"+filepath.Base(saved))
		}
	}
	rec := imageHistoryRecord{
		ID:      fmt.Sprintf("%d", time.Now().UnixNano()),
		Model:   model,
		Prompt:  prompt,
		Size:    size,
		Time:    time.Now().UnixMilli(),
		Local:   local,
		Sources: urls,
	}
	g.imgHis = append([]imageHistoryRecord{rec}, g.imgHis...)
	if len(g.imgHis) > 200 {
		g.imgHis = g.imgHis[:200]
	}
	g.persistImageHistory()
	return local
}

// downloadImage 把上游临时图片下载到本地 images 目录，返回本地完整路径；成功后清理掉已过后缀
func (g *Gateway) downloadImage(rawURL string) (string, string) {
	ext := ".png"
	if u, err := url.Parse(rawURL); err == nil {
		low := strings.ToLower(u.Path)
		if i := strings.LastIndex(low, "."); i >= 0 {
			if tail := low[i:]; len(tail) >= 4 && len(tail) <= 6 && strings.Trim(tail, "abcdefghijklmnopqrstuvwxyz0123456789") == "" {
				ext = tail
			}
		}
	}
	// 用时间戳命名避免冲突
	local := filepath.Join(g.imgDir, fmt.Sprintf("img_%d%s", time.Now().UnixNano(), ext))
	resp, err := g.client.Get(rawURL)
	if err != nil {
		return rawURL, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rawURL, ""
	}
	f, err := os.Create(local)
	if err != nil {
		return rawURL, ""
	}
	_, cpErr := io.Copy(f, resp.Body)
	f.Close()
	if cpErr != nil {
		_ = os.Remove(local)
		return rawURL, ""
	}
	return rawURL, local
}

// handleImageHistory GET 返回历史列表；POST 保存一条生图记录
func (g *Gateway) handleImageHistory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		g.imgMu.Lock()
		hist := append([]imageHistoryRecord{}, g.imgHis...)
		g.imgMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "records": hist})
	case http.MethodPost:
		var in struct {
			Model  string   `json:"model"`
			Prompt string   `json:"prompt"`
			Size   string   `json:"size"`
			URLs   []string `json:"urls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
			return
		}
		if len(in.URLs) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少生成图片地址")
			return
		}
		local := g.saveImageRecord(in.Model, in.Prompt, in.Size, in.URLs)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "local": local})
	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 GET/POST")
	}
}

// handleImageFile 提供本地保存的生图文件访问
func (g *Gateway) handleImageFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(strings.TrimPrefix(r.URL.Path, "/admin/image-file/"))
	if name == "" || name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}
	fp := filepath.Join(g.imgDir, name)
	http.ServeFile(w, r, fp)
}

// handleImageHistoryDelete 按 ID 删除单条生图记录
func (g *Gateway) handleImageHistoryDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少 id")
		return
	}
	g.imgMu.Lock()
	g.imgHis = g.removeImageRecord(id)
	g.persistImageHistory()
	g.imgMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handleImageHistoryClear 清空全部生图记录
func (g *Gateway) handleImageHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	g.imgMu.Lock()
	g.imgHis = []imageHistoryRecord{}
	g.persistImageHistory()
	entries, _ := os.ReadDir(g.imgDir)
	for _, e := range entries {
		if !e.IsDir() {
			_ = os.Remove(filepath.Join(g.imgDir, e.Name()))
		}
	}
	g.imgMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// removeImageRecord 从历史中移除指定 ID 的记录，并删除其本地图片文件
func (g *Gateway) removeImageRecord(id string) []imageHistoryRecord {
	out := make([]imageHistoryRecord, 0, len(g.imgHis))
	for _, rec := range g.imgHis {
		if rec.ID == id {
			for _, l := range rec.Local {
				_ = os.Remove(filepath.Join(g.imgDir, filepath.Base(l)))
			}
		} else {
			out = append(out, rec)
		}
	}
	return out
}



// forward 核心转发逻辑：Auto 模式可在多个模型间自动降级，每个模型内再对多账号轮询，
// 遇限流/超时/5xx 立即切换。
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, upstreamPath string, body []byte) {
	model := extractModel(body)
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体中缺少 model 字段")
		return
	}

	isStream := extractStream(body)

	// 针对 Trae 等对上游 SSE 解析异常的客户端：强制转非流式，返回完整 JSON
	g.mu.RLock()
	disableStream := g.cfg.DisableStream
	maxTokensLimit := g.cfg.MaxTokensLimit
	g.mu.RUnlock()

	if disableStream && isStream {
		if nb, ok := disableStreamInBody(body); ok {
			body = nb
			isStream = false
		}
	}

	// 确定候选模型：普通模式仅一个；Auto 模式按优先级列出全部对话模型
	var models []string
	if model == autoModelID {
		models = g.pickAutoModelList()
		if len(models) == 0 {
			writeError(w, http.StatusTooManyRequests, "quota_exceeded_error",
				"Auto 模式：所有对话模型当前均不可用，请稍后再试")
			return
		}
		log.Printf("[Auto] 候选模型 %v", models)
	} else {
		models = []string{model}
	}

	log.Printf("[请求体] model=%s stream=%v body=%s", model, isStream, truncate(body, 600))

	var lastStatus int
	var lastErr string
	for _, m := range models {
		if model == autoModelID {
			body = replaceModelInBody(body, m)
			log.Printf("[Auto] 尝试模型 %s", m)
		}
		status, errMsg, done := g.forwardOneModel(w, r, upstreamPath, body, m, isStream, maxTokensLimit, model == autoModelID)
		if done {
			return
		}
		lastStatus = status
		lastErr = errMsg
	}

	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	errType := "server_error"
	if lastStatus == http.StatusTooManyRequests {
		errType = "quota_exceeded_error"
	} else if lastStatus == http.StatusNotFound {
		errType = "not_found_error"
	}
	writeError(w, lastStatus, errType, "模型 "+model+" 全部尝试失败: "+lastErr)
}

// forwardOneModel 针对单个模型做多账号轮询转发。成功透传后 done=true；
// 否则返回状态码与错误信息（该模型所有账号均不可用或均失败）。
// auto=true 表示处于 Auto 降级循环中，404 等模型级错误会切换到下一个候选模型。
func (g *Gateway) forwardOneModel(w http.ResponseWriter, r *http.Request, upstreamPath string, body []byte, model string, isStream bool, maxTokensLimit int, auto bool) (status int, errMsg string, done bool) {
	g.mu.RLock()
	total := len(g.modelIndex[model])
	g.mu.RUnlock()

	if total == 0 {
		return http.StatusNotFound, fmt.Sprintf("网关未配置模型 %s 对应的账号，请在配置中为账号添加该模型", model), false
	}

	var lastErr string
	lastStatus := http.StatusBadGateway
	for attempt := 0; attempt < total; attempt++ {
		ai, ok := g.nextAccount(model)
		if !ok {
			return http.StatusTooManyRequests, "模型 " + model + " 的所有账号均处于冷却中，请稍后再试", false
		}
		g.markCallStart(ai, model)

		// 额度限流：若该 key 在当前窗口内已用满免费额度，跳过并切换下一个
		if !g.tryConsumeQuota(ai, model) {
			g.markCallEnd(ai, model)
			g.mu.RLock()
			alias := ""
			if ai < len(g.cfg.Accounts) {
				alias = g.cfg.Accounts[ai].Alias
			}
			g.mu.RUnlock()
			log.Printf("[网关] 账号[%s] 模型[%s] 免费额度已用满，冷却到窗口结束，切换下一个账号", alias, model)
			continue
		}

		g.mu.RLock()
		if ai >= len(g.cfg.Accounts) {
			g.mu.RUnlock()
			g.markCallEnd(ai, model)
			continue
		}
		acc := g.cfg.Accounts[ai]
		g.mu.RUnlock()

		resp, err := g.doRequest(r, acc.ApiKey, upstreamPath, body, model, isStream, maxTokensLimit)
		if err != nil {
			g.markCallEnd(ai, model)
			g.stats.record("fail")
			lastErr = err.Error()
			lastStatus = http.StatusBadGateway
			g.cooldown(ai, model, "请求异常: "+err.Error(), g.cfg.CooldownSeconds)
			g.saveCooldownState()
			log.Printf("[网关] 账号[%s] 模型[%s] 请求异常，切换下一个账号: %v", acc.Alias, model, err)
			continue
		}

		// 429 限流 / 408 取消 / 5xx 服务端错误 -> 切换下一个账号
		if resp.StatusCode == 429 || resp.StatusCode == 408 || resp.StatusCode >= 500 {
			g.markCallEnd(ai, model)
			if resp.StatusCode == 429 {
				g.stats.record("rate_429")
			} else {
				g.stats.record("fail")
			}
			hint := readErrorHint(resp)
			_ = resp.Body.Close()
			lastErr = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, hint)
			lastStatus = resp.StatusCode
			cd := g.cfg.CooldownSeconds
			if resp.StatusCode == 429 {
				// 429 使用指数退避：连续被限流时冷却时间逐次翻倍，
				// 避免「冷却 3 分钟就恢复显示可用、一调用又 429」的反复循环。
				cd = g.backoffCooldown(ai, model)
			}
			g.cooldown(ai, model, fmt.Sprintf("HTTP %d %s", resp.StatusCode, hint), cd)
			g.saveCooldownState()
			log.Printf("[网关] 账号[%s] 模型[%s] HTTP %d，冷却 %d 秒，切换下一个账号", acc.Alias, model, resp.StatusCode, cd)
			continue
		}

		// 404 模型不存在 -> 说明该模型无法服务当前接口。
		// 在 Auto 模式下应切换下一个候选模型，而不是把错误直接透传给客户端。
		// 例如图像模型 u1-fast / u1.5-lite 走 /chat/completions 时上游返回 404。
		if resp.StatusCode == 404 {
			g.markCallEnd(ai, model)
			g.stats.record("fail")
			hint := readErrorHint(resp)
			_ = resp.Body.Close()
			// 图像模型本身不支持对话接口，给出明确原因，避免 Trae 只看到「model is not found」
			if isImageModel(model) {
				msg := "模型 " + model + " 是图像生成模型，仅支持图像接口（/v1/images/generations），无法用于文本对话，因此不能添加到 Trae 等对话客户端"
				log.Printf("[网关] 模型[%s] 图像模型不支持对话接口", model)
				if auto {
					continue
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"message": msg,
						"type":    "invalid_request_error",
						"model":   model,
					},
				})
				return http.StatusNotFound, msg, true
			}
			lastErr = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, hint)
			if auto {
				log.Printf("[网关] 模型[%s] HTTP %d 不支持当前接口，切换下一个模型", model, resp.StatusCode)
				continue
			}
			// 非 Auto 模式：原样透传错误给客户端
			log.Printf("[转发] 账号[%s] 模型[%s] -> HTTP %d", acc.Alias, model, resp.StatusCode)
			resp.Header.Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"message": hint,
					"type":    "not_found_error",
					"model":   model,
				},
			})
			return resp.StatusCode, lastErr, true
		}

		// 其余（200 成功、401/403 等业务错误）原样透传
		log.Printf("[转发] 账号[%s] 模型[%s] -> HTTP %d", acc.Alias, model, resp.StatusCode)
		g.stats.record("success")
		// 成功调用后清零连续 429 计数，恢复基础冷却时间
		g.mu.Lock()
		g.stateFor(ai, model).consec429 = 0
		g.mu.Unlock()
		g.saveCooldownState()
		modelForUsage := model
		gw := g
		copyResponse(w, resp, isStream, func(p, c int64) {
			gw.usage.add(p, c)
			gw.stats.recordUsage(modelForUsage, p, c)
			log.Printf("[用量] 模型[%s] prompt=%d completion=%d", modelForUsage, p, c)
		})
		g.markCallEnd(ai, model)
		return 0, "", true
	}

	return lastStatus, "模型 " + model + " 全部账号尝试失败: " + lastErr, false
}

// backoffCooldown 计算 429 限流后的指数退避冷却秒数。
// 连续 429 次数越多，冷却越久（10、20、40、80、160、300 分钟封顶），
// 让账号在真正恢复前保持「冷却中」，避免反复显示可用又反复被 429。
func (g *Gateway) backoffCooldown(ai int, model string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateFor(ai, model)
	st.consec429++
	base := g.cfg.Cooldown429
	secs := base
	mul := 1
	for i := 0; i < st.consec429-1 && i < 5; i++ {
		mul *= 2
	}
	secs = base * mul
	if secs > 18000 {
		secs = 18000
	}
	log.Printf("[网关] 账号[%s] 模型[%s] 连续 429 %d 次，退避冷却 %d 秒", g.cfg.Accounts[ai].Alias, model, st.consec429, secs)
	return secs
}

func (g *Gateway) doRequest(r *http.Request, apiKey, path string, body []byte, model string, stream bool, maxTokensLimit int) (*http.Response, error) {
	body = stripReasoning(body, model)
	body = limitMaxTokens(body, maxTokensLimit)
	body = g.forceNoToolChoice(body, model)
	if stream {
		body = ensureStreamUsage(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.cfg.UpstreamBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	c := g.client
	if stream {
		c = g.streamClient
	}
	return c.Do(req)
}

// copyResponse 把上游响应透传给客户端，支持流式 flush，并从响应体中提取 usage token 消耗
func copyResponse(w http.ResponseWriter, resp *http.Response, stream bool, onUsage func(prompt, completion int64)) {
	for k, vs := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var usageBuf []byte // 累积末尾数据用于解析 usage
	const maxUsageScan = 256 * 1024
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if onUsage != nil {
				usageBuf = append(usageBuf, buf[:n]...)
				if len(usageBuf) > maxUsageScan {
					usageBuf = usageBuf[len(usageBuf)-maxUsageScan:]
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				_ = resp.Body.Close()
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()
	if onUsage != nil && resp.StatusCode == 200 {
		p, c := extractUsage(usageBuf, stream)
		if p > 0 || c > 0 {
			onUsage(p, c)
		}
	}
}

// extractUsage 从响应体（JSON 或 SSE）中解析 usage 的 prompt/completion token
func extractUsage(body []byte, stream bool) (int64, int64) {
	var p, c int64
	if stream {
		// SSE：每个 data: 行是一条 JSON，usage 通常出现在末尾块
		lines := bytes.Split(body, []byte("\n"))
		for _, ln := range lines {
			ln = bytes.TrimSpace(ln)
			if !bytes.HasPrefix(ln, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(ln, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) || len(payload) == 0 {
				continue
			}
			up, uc := parseUsageJSON(payload)
			if up > p {
				p = up
			}
			if uc > c {
				c = uc
			}
		}
	} else {
		p, c = parseUsageJSON(body)
	}
	return p, c
}

func parseUsageJSON(data []byte) (int64, int64) {
	var m struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &m) != nil {
		return 0, 0
	}
	return m.Usage.PromptTokens, m.Usage.CompletionTokens
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length",
}

func isHopHeader(h string) bool {
	for _, k := range hopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

func readErrorHint(resp *http.Response) string {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return ""
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(data))
}

func (g *Gateway) handleUsage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	u := g.usage.totals()
	// 按每日实际模型分布计算精确省钱金额，而非用今天的混合费率套全部历史
	dailySaved, fbIn, fbOut := g.stats.savedByPeriod(g.cfg.ModelPrices)
	todaySaved, monthSaved, yearSaved, totalSaved := g.usage.computeSaved(dailySaved, fbIn, fbOut)
	savedMap := map[string]float64{
		"today":  todaySaved,
		"month":  monthSaved,
		"year":   yearSaved,
		"total":  totalSaved,
	}
	for _, key := range []string{"today", "year", "month", "total"} {
		if m, ok := u[key].(map[string]interface{}); ok {
			m["saved"] = savedMap[key]
		}
	}
	if dm, ok := u["daily"].([]map[string]interface{}); ok {
		for _, day := range dm {
			date, _ := day["date"].(string)
			if v, ok := dailySaved[date]; ok {
				day["saved"] = v
			} else {
				p, _ := day["prompt"].(int64)
				c, _ := day["completion"].(int64)
				day["saved"] = float64(p)/1e6*fbIn + float64(c)/1e6*fbOut
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"usage": u,
	})
}

func (g *Gateway) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"stats": g.stats.snapshot(g.cfg.ModelPrices),
	})
}

func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	g.mu.RLock()
	models := modelSet(g.cfg)
	g.mu.RUnlock()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]interface{}{"id": m, "object": "model", "owned_by": "upstream"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	now := time.Now()
	type modelState struct {
		Model       string `json:"model"`
		FailCount   int    `json:"fail_count"`
		Cooling     bool   `json:"cooling"`
		CoolRemain  int    `json:"cooldown_remain_seconds"`
		ActiveCalls int    `json:"active_calls"` // 当前进行中的调用数（>0 表示使用中）
	}
	type accInfo struct {
		Index      int          `json:"index"`
		Alias      string       `json:"alias"`
		KeyTail    string       `json:"key_tail"`
		CallCount  int64        `json:"call_count"`
		LastCall   int64        `json:"last_call"` // Unix 秒，0 表示从未调用
		Active     int          `json:"active_calls"`
		Status     string       `json:"status"` // in_use / cooling / ready
		CoolRemain int          `json:"cooldown_remain_seconds"`
		Models     []modelState `json:"models"`
	}
	out := make([]accInfo, 0, len(g.cfg.Accounts))
	for i, acc := range g.cfg.Accounts {
		ms := make([]modelState, 0, len(acc.Models))
		var (
			totalCalls  int64
			lastCall    int64
			totalActive int
			anyCooling  bool
			maxRemain   int
		)
		for _, m := range acc.Models {
			st := g.states[stateKey(i, strings.TrimSpace(m))]
			if st == nil {
				st = &accountState{}
			}
			remain := int(time.Until(st.disableUntil).Seconds())
			if remain < 0 {
				remain = 0
			}
			cooling := now.Before(st.disableUntil)
			ms = append(ms, modelState{
				Model:       m,
				FailCount:   st.failCount,
				Cooling:     cooling,
				CoolRemain:  remain,
				ActiveCalls: st.activeCalls,
			})
			totalCalls += st.callCount
			if st.lastCall.Unix() > lastCall {
				lastCall = st.lastCall.Unix()
			}
			totalActive += st.activeCalls
			if cooling {
				anyCooling = true
				if remain > maxRemain {
					maxRemain = remain
				}
			}
		}
		status := "ready"
		if totalActive > 0 {
			status = "in_use"
		} else if anyCooling {
			status = "cooling"
		}
		keyTail := acc.ApiKey
		if len(keyTail) > 6 {
			keyTail = "..." + keyTail[len(keyTail)-4:]
		}
		out = append(out, accInfo{
			Index:      i,
			Alias:      acc.Alias,
			KeyTail:    keyTail,
			CallCount:  totalCalls,
			LastCall:   lastCall,
			Active:     totalActive,
			Status:     status,
			CoolRemain: maxRemain,
			Models:     ms,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts": out,
		"models":   modelSet(g.cfg),
	})
}

func (g *Gateway) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"name":    "多账号 AI 网关",
		"version": "1.1.0",
		"endpoints": []string{
			"POST /v1/chat/completions",
			"POST /v1/images/generations",
			"GET  /v1/models",
			"GET  /health",
			"GET  /admin",
		},
	})
}

// handleAdminPage 返回配置管理网页
func (g *Gateway) handleAdminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, adminHTML)
}

// handleAdminConfig 返回当前配置（供网页加载）
func (g *Gateway) handleAdminConfig(w http.ResponseWriter, _ *http.Request) {
	g.mu.RLock()
	cfg := g.cfg
	g.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}

// handleAdminSave 接收网页提交的配置，校验后写回文件并热重载
func (g *Gateway) handleAdminSave(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "配置 JSON 解析失败: "+err.Error())
		return
	}
	applyDefaults(&cfg)
	if len(cfg.Accounts) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "至少需要一个账号")
		return
	}
	for i, acc := range cfg.Accounts {
		if strings.TrimSpace(acc.ApiKey) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("第 %d 个账号缺少 api_key", i+1))
			return
		}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "序列化配置失败: "+err.Error())
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "写入配置文件失败: "+err.Error())
		return
	}
	g.reload(cfg)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       true,
		"accounts": len(cfg.Accounts),
		"models":   modelSet(cfg),
	})
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    typ,
		},
	})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type loggingWriter struct {
	http.ResponseWriter
	status int
}

func (lw *loggingWriter) WriteHeader(code int) {
	lw.status = code
	lw.ResponseWriter.WriteHeader(code)
}

// logging 记录每个请求的方法、路径、状态码与耗时，便于诊断
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(lw, r)
		log.Printf("[请求] %s %s -> %d (%s)", r.Method, r.URL.Path, lw.status, time.Since(start).Round(time.Millisecond))
	})
}

// handleRestart 重启网关自身。
// 通过 /admin/restart 调用：用当前可执行文件「脱离父进程」的方式重新拉起，
// 然后立即关闭当前 HTTP 服务并退出。这样即使网关以 SYSTEM/Session0 身份运行，
// 也能用同一身份重启，且新进程不会随旧进程一起退出。
func handleRestart(w http.ResponseWriter, r *http.Request) {
	exePath, err := os.Executable()
	if err != nil {
		http.Error(w, "无法确定可执行文件路径: "+err.Error(), http.StatusInternalServerError)
		return
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		http.Error(w, "无法确定可执行文件绝对路径: "+err.Error(), http.StatusInternalServerError)
		return
	}
	wd, _ := os.Getwd()

	// 关键：Windows CreateProcess 构造命令行时，第 0 项必须是可执行文件名，
	// 否则新进程会因参数错位而无法正确启动（这正是「重启后网关不回来」的根因）。
	// 这里显式用完整 exe 路径作为 Args[0]，并带上原参数（如 -config）。
	args := append([]string{exePath}, os.Args[1:]...)

	// Windows：新开控制台 + 脱离进程组，保证旧进程退出后新进程独立存活，
	// 不会因为继承旧的 job/console 而被一起终结。
	const (
		createNewConsole    = 0x00000010
		createNewProcessGrp = 0x00000200
	)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"ok":true,"msg":"网关正在重启，约 2 秒后可重新访问"}`))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 关键：必须先优雅关闭 HTTP server 释放端口，再启动新进程，
	// 否则新进程会因「端口仍被旧进程占用」bind 失败而退出 —— 这正是之前
	// 「点重启后网关只关闭、起不来」的根因。
	go func() {
		// 先让响应写回浏览器
		time.Sleep(200 * time.Millisecond)
		// 1) 优雅关闭，释放 18888 端口
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if srv != nil {
			if err := srv.Shutdown(ctx); err != nil {
				log.Printf("[重启] 优雅关闭失败，强制关闭: %v", err)
				_ = srv.Close()
			}
		}
		cancel()
		// 2) 确认端口已释放（最多等 2 秒），避免新进程 bind 冲突
		for i := 0; i < 20; i++ {
			ln, err := net.Listen("tcp", srv.Addr)
			if err == nil {
				_ = ln.Close()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		// 3) 端口已空，启动新进程
		cmd := &exec.Cmd{
			Path:        exePath,
			Args:        args,
			Dir:         wd,
			SysProcAttr: &syscall.SysProcAttr{CreationFlags: createNewConsole | createNewProcessGrp},
		}
		if err := cmd.Start(); err != nil {
			log.Printf("[重启] 拉起新进程失败: %v", err)
		} else {
			log.Printf("[重启] 已拉起新进程 PID=%d", cmd.Process.Pid)
		}
		time.Sleep(300 * time.Millisecond)
		log.Printf("[重启] 旧进程退出")
		os.Exit(0)
	}()
}

func main() {
	// 默认使用可执行文件同目录下的 config.json，避免开机自启动时工作目录不确定导致找不到配置
	exePath, err := os.Executable()
	defaultConfig := "config.json"
	if err == nil {
		exeDir := filepath.Dir(exePath)
		defaultConfig = filepath.Join(exeDir, "config.json")
		// 同时把日志写到网关目录下的 gateway.log，方便后台/自启动模式下排查
		if lf, lerr := os.OpenFile(filepath.Join(exeDir, "gateway.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); lerr == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, lf))
		}
	}
	configPath := flag.String("config", defaultConfig, "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败 %s: %v", *configPath, err)
	}
	exeDir := filepath.Dir(*configPath)
	g := newGateway(cfg, filepath.Join(exeDir, "usage.json"))
	g.imgHisPath = filepath.Join(exeDir, "image_history.json")
	g.imgDir = filepath.Join(exeDir, "images")
	_ = os.MkdirAll(g.imgDir, 0755)
	g.loadImageHistory()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", g.handleHealth)
	mux.HandleFunc("/usage", g.handleUsage)
	mux.HandleFunc("/stats", g.handleStats)
	mux.HandleFunc("/v1/models", g.handleModels)
	mux.HandleFunc("/v1/chat/completions", g.handleChat)
	mux.HandleFunc("/v1/images/generations", g.handleImages)
	// 兼容部分客户端把 base_url 填成 /v1 的情况：把 POST /v1 也按对话转发
	mux.HandleFunc("/v1", g.handleChat)
	// 配置管理网页
	mux.HandleFunc("/admin", g.handleAdminPage)
	mux.HandleFunc("/admin/config", g.handleAdminConfig)
	mux.HandleFunc("/admin/save", func(w http.ResponseWriter, r *http.Request) {
		g.handleAdminSave(w, r, *configPath)
	})
	mux.HandleFunc("/admin/image-history", g.handleImageHistory)
	mux.HandleFunc("/admin/image-history/delete", g.handleImageHistoryDelete)
	mux.HandleFunc("/admin/image-history/clear", g.handleImageHistoryClear)
	mux.HandleFunc("/admin/image-file/", g.handleImageFile)
	mux.HandleFunc("/admin/restart", handleRestart)
	mux.HandleFunc("/", g.handleRoot)

	log.Printf("多账号 AI 网关启动: http://%s  (上游 %s)", cfg.Listen, cfg.UpstreamBase)
	log.Printf("已注册账号 %d 个，模型 %d 个: %s", len(cfg.Accounts), len(modelSet(cfg)), strings.Join(modelSet(cfg), ", "))
	log.Printf("配置管理页面: http://%s/admin", cfg.Listen)
	srv = &http.Server{Addr: cfg.Listen, Handler: logging(cors(mux))}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
