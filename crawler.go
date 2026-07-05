package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

// === 公共类型 ===

// CrawlerResult 单条 URL 检测结果 — 推送到 UI 表格的一行。
// 与 Python 版的 CheckResult 字段一一对应(供 mind parity)。
type CrawlerResult struct {
	Domain     string `json:"domain"`     // 起点域名
	URL        string `json:"url"`        // 被测 URL
	SourceURL  string `json:"sourceUrl"`  // 父页面(此链接从该页被发现),空=站点入口
	IsExternal bool   `json:"isExternal"` // 是否起点域名之外的链接
	Status     string `json:"status"`     // "✅ 200" / "❌ timeout"
	Code       int    `json:"code"`       // HTTP code,异常时 -1
	LatencyMs  int    `json:"latencyMs"`
	FinishedAt string `json:"finishedAt"`
}

// CrawlerProgress 整体进度事件:UI 用来更新状态条 / 指示灯。
type CrawlerProgress struct {
	Level   string `json:"level"` // "info" | "warn" | "done"
	Message string `json:"message"`
}

// 浏览器 UA,避开裸 net/http 拦截。
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// 静态资源后缀:爬到 HTML 才挖链接,避免下载巨量 CSS/图片。
var staticExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".svg": true, ".ico": true, ".css": true, ".js": true, ".pdf": true,
	".zip": true, ".rar": true, ".mp4": true, ".mp3": true, ".woff": true,
	".woff2": true, ".ttf": true, ".eot": true, ".xml": true, ".txt": true,
	".json": true,
}

// === 配置 ===

type CrawlerConfig struct {
	Domains         []string
	MaxDepth        int
	Concurrency     int
	TimeoutSec      float64
	DelayMs         int           // 每请求间隔(毫秒),0=不限速
	IncludeExternal bool          // 是否也测外站链接
	OnResult        func(CrawlerResult)
	OnProgress      func(CrawlerProgress)
}

type Crawler struct {
	cfg    CrawlerConfig
	client *http.Client
	stop   chan struct{}
	once   sync.Once
	count  atomic.Int64
}

// NewCrawler 创建爬虫。配置中的回调若为 nil,内部用 no-op。
func NewCrawler(cfg CrawlerConfig) *Crawler {
	if cfg.OnResult == nil {
		cfg.OnResult = func(CrawlerResult) {}
	}
	if cfg.OnProgress == nil {
		cfg.OnProgress = func(CrawlerProgress) {}
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 20
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 8
	}
	if cfg.MaxDepth < 0 {
		cfg.MaxDepth = 0
	}
	transport := &http.Transport{
		TLSHandshakeTimeout: time.Duration(cfg.TimeoutSec) * time.Second,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: cfg.Concurrency + 5,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Crawler{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.TimeoutSec) * time.Second,
		},
		stop: make(chan struct{}),
	}
}

// Stop 取消进行中的爬取(幂等)。
func (c *Crawler) Stop() {
	c.once.Do(func() { close(c.stop) })
}

// === 爬取主入口 ===

// Run 在当前 goroutine 同步跑;通常用 go c.Run(ctx) 异步。
func (c *Crawler) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("crawler panic: %v", r)
			c.cfg.OnProgress(CrawlerProgress{Level: "warn", Message: fmt.Sprintf("crashed: %v", r)})
		}
	}()

	if len(c.cfg.Domains) == 0 {
		c.cfg.OnProgress(CrawlerProgress{Level: "warn", Message: "未提供有效域名"})
		return
	}

	for _, d := range c.cfg.Domains {
		c.runOne(ctx, d)
		select {
		case <-c.stop:
			c.cfg.OnProgress(CrawlerProgress{
				Level:   "warn",
				Message: fmt.Sprintf("⏹ 已停止 · 共 %d 条", c.count.Load()),
			})
			return
		default:
		}
	}

	c.cfg.OnProgress(CrawlerProgress{
		Level:   "done",
		Message: fmt.Sprintf("✅ 全部完成 · 共 %d 条", c.count.Load()),
	})
}

// runOne 处理单个起点域名:同站递归爬 + 外站可选只测一次。
func (c *Crawler) runOne(ctx context.Context, base string) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		c.cfg.OnProgress(CrawlerProgress{Level: "warn", Message: "无法解析: " + base})
		return
	}
	baseHost := strings.ToLower(parsed.Host)

	// 队列项:同站:(url, depth, source)
	type sameItem struct {
		url    string
		depth  int
		source string
	}
	// 外站队列:(url, source)
	type extItem struct {
		url    string
		source string
	}

	sameQ := []sameItem{{url: base, depth: 0, source: ""}}
	extQ := []extItem{}
	visited := make(map[string]bool)
	var vmu sync.Mutex

	// 全局并发闸:同时最多 cfg.Concurrency 个 HTTP 请求在飞
	sem := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup

	emit := func(r CrawlerResult) {
		r.Domain = base
		c.cfg.OnResult(r)
	}

	tryEmit := func(target string, source string, depth int, isExt bool) {
		// dedup + visible
		vmu.Lock()
		if visited[target] {
			vmu.Unlock()
			return
		}
		visited[target] = true
		vmu.Unlock()

		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			// 用户中止
			select {
			case <-c.stop:
				return
			case <-ctx.Done():
				return
			default:
			}

			// 间隔限速
			if c.cfg.DelayMs > 0 {
				select {
				case <-time.After(time.Duration(c.cfg.DelayMs) * time.Millisecond):
				case <-c.stop:
					return
				case <-ctx.Done():
					return
				}
			}

			code, latency := c.probe(target)
			now := time.Now().Format("15:04:05")

			var label string
			switch {
			case code < 0:
				label = "❌ timeout/error"
			case code >= 200 && code < 400:
				label = fmt.Sprintf("✅ %d", code)
			default:
				label = fmt.Sprintf("❌ %d", code)
			}

			c.count.Add(1)
			emit(CrawlerResult{
				Domain:     base,
				URL:        target,
				SourceURL:  source,
				IsExternal: isExt,
				Status:     label,
				Code:       code,
				LatencyMs:  latency,
				FinishedAt: now,
			})

			// 仅同站、200 且像 HTML 才继续挖链接
			if isExt || code < 200 || code >= 300 || depth > c.cfg.MaxDepth || hasStaticExt(target) {
				return
			}

			htmlBody, ok := c.fetchHTML(target)
			if !ok {
				return
			}
			links := extractLinks(htmlBody, target)
			for _, link := range links {
				lh := hostOf(link)
				if lh == "" {
					continue
				}
				if lh == baseHost {
					if depth+1 <= c.cfg.MaxDepth {
						vmu.Lock()
						duplicate := visited[link]
						vmu.Unlock()
						if !duplicate {
							sameQ = append(sameQ, sameItem{url: link, depth: depth + 1, source: target})
						}
					}
				} else if c.cfg.IncludeExternal {
					vmu.Lock()
					duplicate := visited[link]
					vmu.Unlock()
					if !duplicate {
						extQ = append(extQ, extItem{url: link, source: target})
					}
				}
			}
		}()
	}

	// 同站 + 外站 并行推进,直到两队列都空
	//
	// 关键:子链接是在 goroutine 内 append 到 sameQ/extQ 的,主循环如果只读
	// 队列长度退出,会在 base goroutine 还没把链接入队时就退出。
	// 修复:循环里先 wg.Wait() 把当前所有活跃 goroutine 跑完,再判断队列。
	for {
		select {
		case <-c.stop:
			wg.Wait()
			return
		default:
		}

		// 等所有正在跑的 goroutine 写完他们发现的链接
		wg.Wait()

		// 现在队列稳定了,判断退出
		if len(sameQ) == 0 && len(extQ) == 0 {
			break
		}

		// 优先消费外站这一批(不递归)
		batch := 0
		for len(extQ) > 0 && batch < c.cfg.Concurrency {
			it := extQ[0]
			extQ = extQ[1:]
			tryEmit(it.url, it.source, 0, true)
			batch++
		}

		if len(sameQ) > 0 {
			it := sameQ[0]
			sameQ = sameQ[1:]
			tryEmit(it.url, it.source, it.depth, false)
		}
	}
}

// === 工具函数 ===

// probe 做 HEAD,405/501 退回 GET。
func (c *Crawler) probe(target string) (code int, latencyMs int) {
	t0 := time.Now()
	req, err := http.NewRequest("HEAD", target, nil)
	if err != nil {
		return -1, int(time.Since(t0).Milliseconds())
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != 501 {
			return resp.StatusCode, int(time.Since(t0).Milliseconds())
		}
	}
	// 退回 GET
	t0 = time.Now()
	req2, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return -1, int(time.Since(t0).Milliseconds())
	}
	req2.Header.Set("User-Agent", browserUA)
	resp, err = c.client.Do(req2)
	if err != nil {
		return -1, int(time.Since(t0).Milliseconds())
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, int(time.Since(t0).Milliseconds())
}

// fetchHTML GET target 取 HTML 内容(用于解析链接)。
func (c *Crawler) fetchHTML(target string) (string, bool) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "html") {
		return "", false
	}
	limited := io.LimitReader(resp.Body, 2*1024*1024) // 限 2MB
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// extractLinks 从 HTML body 提取所有非 fragment / data: 的链接。
func extractLinks(body, base string) []string {
	out := []string{}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return out
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return out
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "a" || n.Data == "link") {
			for _, attr := range n.Attr {
				if attr.Key != "href" {
					continue
				}
				href := strings.TrimSpace(attr.Val)
				if href == "" {
					break
				}
				low := strings.ToLower(href)
				if strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "mailto:") ||
					strings.HasPrefix(low, "tel:") || strings.HasPrefix(low, "#") {
					break
				}
				abs, err := baseURL.Parse(href)
				if err != nil || abs.Host == "" {
					break
				}
				// 去 fragment
				abs.Fragment = ""
				out = append(out, abs.String())
				break
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return out
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func hasStaticExt(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	low := strings.ToLower(u.Path)
	for ext := range staticExts {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}
