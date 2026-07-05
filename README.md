# Web URL Health Checker

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Made with Wails](https://img.shields.io/badge/Made%20with-Wails-2-red)](https://wails.io)
[![Go 1.23+](https://img.shields.io/badge/Go-1.23+-00ADD8)](https://go.dev)

一个**桌面工具**:输入域名,自动同站爬取所有链接,对每个 URL 测一次 HTTP 状态,
**实时**显示哪些是死链、哪个页面引用了死链。

**单文件可执行**(无运行时、无安装、无 Python 环境),**macOS** 和 **Windows** 双平台原生打包。

## 功能

- **多域名输入**,同站爬取(深度可调 0~5 层)
- **来源追溯**:每条死链记录"从哪个父页发现" — 鼠标悬浮即可看到,一键跳过去修
- **🔗 一键打开**:表格里每行的小图标,直接用系统浏览器打开该 URL 或来源页
- **智能滚动**:跟微信/Telegram 同款 — 你停在底部自动跟新结果,你滑开就停,回到底继续
- **间隔限速**:每请求间隔 N 毫秒,免被目标 IP 拉黑
- **可选外站测**:勾选后,页面里外站链接也测 1 次(浅黄色背景标记)
- **并发 1-200 / 超时 1-60s**,实时统计 ✅/❌ 计数

## 📬 联系方式

- **QQ**: 496631085
- **GitHub**: [@xiaohe4966](https://github.com/xiaohe4966)

## 📦 下载与运行

去 [Releases](https://github.com/xiaohe4966/web-url-health-checker/releases) 页面下载对应平台:

| 文件 | 适用 |
|---|---|
| `web-url-health-checker.exe` | Windows 10/11 x86-64(无需安装,双击运行)|
| `web-url-health-checker.app.zip` | macOS 10.15+ Universal(macOS 上要先解压)|

**Windows**:双击 `.exe` 即可,程序会要求 WebView2 Runtime(Win10/11 默认装了;没有的话第一次启动会提示下载,微软官方组件)。

**macOS**:解压 .app.zip 后,首次启动会触发 Gatekeeper,**右键 → 打开**即可,或 `xattr -cr web-url-health-checker.app` 然后正常双击。

## 🛠 本地开发

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest   # 装 wails CLI
wails dev   # 实时预览,改前端自动热重载
wails build -platform windows/amd64    # 出 .exe(可在 macOS 上交叉编译)
wails build                              # 出当前平台 .app
```

只需 Go + Wails。无需 Node、无需 Python、无需任何运行时。

## 架构

```
.
├── main.go              # Wails 入口
├── app.go               # 暴露给前端的 App struct
├── crawler.go           # 纯 Go 爬取核心(零外部依赖,只用 stdlib + golang.org/x/net)
├── frontend/
│   └── dist/
│       └── index.html   # Vanilla JS 单文件前端(无构建步骤)
├── wails.json
├── go.mod / go.sum
└── README.md / LICENSE
```

**前后端通信**:
- Go → 前端:`runtime.EventsEmit(ctx, "result"|"progress"|"finished", payload)`
- 前端 → Go:`window.go.main.App.Start(...)` / `App.Stop()`

爬虫核心跟 Wails 完全解耦,可单测、可单独复用。

## License

MIT © 2026 xiaohe4966
