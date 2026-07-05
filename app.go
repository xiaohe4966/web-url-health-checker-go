package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App 是 wails 暴露给前端的"控制器"。
//
// 设计:前端用 Wails 生成的 binding 调用本 App 上的方法(Start/Stop/OnXxx)
// 我们内部用 wails runtime 把 Go 的事件 emit 到前端。
type App struct {
	ctx context.Context

	mu       sync.Mutex
	crawler  *Crawler
	runCount int // 每次 Start 递增,旧 goroutine 自己退出(防止 race)
}

// NewApp 创建一个准备好的 App。
func NewApp() *App { return &App{} }

// startup 是 Wails 在 webview 起来后调用的钩子,保存 ctx 供后续 runtime 调用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// Start 启动一次爬取任务。重复调用会取消旧的、起新的。
// 参数:
//
//	domains         []string — 待爬的入口域名
//	maxDepth        int     — 0..5
//	concurrency     int     — 1..200
//	timeoutSec      float64 — 单请求超时
//	delayMs         int     — 0..10000,每请求间隔
//	includeExternal bool    — 是否也测外站
func (a *App) Start(domains []string, maxDepth, concurrency int, timeoutSec float64,
	delayMs int, includeExternal bool) error {

	a.mu.Lock()
	if a.crawler != nil {
		a.crawler.Stop()
	}
	runID := a.runCount
	a.runCount++
	a.mu.Unlock()

	if len(domains) == 0 {
		a.emitProgress("warn", "请输入至少一个域名")
		return fmt.Errorf("no domains")
	}

	c := NewCrawler(CrawlerConfig{
		Domains:         domains,
		MaxDepth:        maxDepth,
		Concurrency:     concurrency,
		TimeoutSec:      timeoutSec,
		DelayMs:         delayMs,
		IncludeExternal: includeExternal,
		OnResult: func(r CrawlerResult) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "result", r)
			}
		},
		OnProgress: func(p CrawlerProgress) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "progress", p)
			}
		},
	})

	a.mu.Lock()
	a.crawler = c
	a.mu.Unlock()

	// 异步跑,不阻塞 UI 线程
	go func(rid int, self *Crawler) {
		ctx := a.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		self.Run(ctx)
		// 完成后:仅清掉当前仍是自己的引用(race-safe)
		a.mu.Lock()
		if a.crawler == self {
			a.crawler = nil
		}
		a.mu.Unlock()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "finished", runID)
		}
	}(runID, c)
	return nil
}

// Stop 中止进行中的爬取。
func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.crawler != nil {
		a.crawler.Stop()
	}
}

// OnReady 是前端等 wails runtime ready 后的调用 — 现在没必要,
func (a *App) OnReady() { emitProgress(a, "info", "ready") }

// === 工具 ===

func (a *App) emitProgress(level, msg string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "progress", CrawlerProgress{
		Level: level, Message: msg,
	})
}

func emitProgress(a *App, level, msg string) { a.emitProgress(level, msg) }
