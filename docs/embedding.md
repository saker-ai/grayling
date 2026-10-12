# 在 Go 服务中嵌入 GoLab

模块路径为 `github.com/godeps/golab`，需要 Go 1.27 或更新版本。`golab.Start` 打开平台存储和 Nacos 底座、建立 API，并启动对账与发布任务；它不会接管宿主进程的 HTTP 监听或信号。宿主负责挂载 Handler、处理错误和关闭服务。

```go
package main

import (
    "context"
    "log"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/godeps/golab"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    appCtx, appCancel := context.WithCancel(context.Background())
    defer appCancel()
    app, err := golab.Start(appCtx, golab.Options{
        SQLitePath:        "data/golab.db",
        AuthSecret:        os.Getenv("GOLAB_AUTH_SECRET"),
        RolloutInterval:   30 * time.Second,
        ReconcileInterval: 30 * time.Second,
        Logger:            log.Default(),
    })
    if err != nil {
        log.Fatal(err)
    }
    if token := app.BootstrapOwnerToken(); token != "" {
        log.Printf("store this new owner token securely: %s", token)
    }

    mux := http.NewServeMux()
    mux.Handle("/api/", app.APIHandler())
    mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
        _, _ = w.Write([]byte("host ok"))
    })
    srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
    serveErr := make(chan error, 1)
    go func() { serveErr <- srv.ListenAndServe() }()

    select {
    case <-ctx.Done():
    case err := <-app.Errors():
        if err != nil {
            log.Printf("GoLab stopped: %v", err)
        }
    case err := <-serveErr:
        log.Printf("HTTP stopped: %v", err)
    }
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    _ = srv.Shutdown(shutdownCtx)
    if err := app.Close(shutdownCtx); err != nil {
        log.Printf("GoLab shutdown: %v", err)
    }
}
```

如需使用 GoLab 内置控制台，让宿主在根路径挂载 `app.Handler()`；这个 Handler 同时包含 `/api/` 和控制台资源。若宿主已有自己的页面，只挂载 `app.APIHandler()`。控制台静态资源使用根路径，直接把完整 Handler 挂到子路径不能正确加载资源。

不设置 `NacosURL` 时，GoLab 在回环地址的随机端口启动内置 gonacos；`app.GonacosAddrs()` 返回实际 HTTP/gRPC 地址。改用外部 Nacos 时，在 `Options` 中设置 `NacosURL` 和 `NacosUsername`/`NacosPassword`，或设置 `NacosToken`。外部模式的数据布局与权限见[外部 Nacos 底座](external-nacos.md)。

`App.Close` 只关闭 GoLab 管理的后台任务、底座和存储；宿主的 HTTP server 需要单独关闭。示例使用独立的 `appCtx`，以便先等待宿主 HTTP 请求结束，再关闭 GoLab 存储。传入 `Start` 的 context 被取消时，GoLab 也会自动清理。`Options.DataFile` 为空表示内存平台存储，`SQLitePath` 和 `PostgresDSN` 分别选择 SQLite、PostgreSQL。`RecoveryPoint` 可在开始后台任务前验证内置 gonacos 与 PostgreSQL 的恢复状态；离线写入/验证恢复点和密钥轮换仍由 CLI 执行。
