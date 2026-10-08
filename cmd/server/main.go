// dockerpull — a Docker image puller with a CLI and a GUI.
//
// Command layout:
//
//	dockerpull [serve] [-port N] [-data-dir PATH] [-dev-proxy URL]
//	dockerpull pull   -i <image> [-a ARCH] [-o DIR] [...]
//	dockerpull search -k <keyword> [--source ID] [...]
//	dockerpull tags   -i <image> [--source ID]
//	dockerpull tasks  [list|show|pause|resume|cancel|retry|rm] [id]
//	dockerpull login  <host> [-u user] [-p pass]     保存私有仓库凭证
//	dockerpull logout <host>                          删除私有仓库凭证
//	dockerpull version
//
// The default action runs the server (the single-binary deployment shape);
// everything else is a subcommand. Use `dockerpull pull` for a one-off
// download without starting a web server.
//
// Desktop delivery lives in cmd/desktop (Wails) and shares the boot wiring
// via internal/app.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/app"
	"github.com/Potterluo/docker-pull-tar/internal/buildinfo"
	"github.com/Potterluo/docker-pull-tar/internal/config"
)

// Stamped by -ldflags at build time (see Makefile / scripts/build.ps1).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Exit codes. The legacy 1ms build returned 0 on every failure, which made
// it unusable in CI; these are explicit and asserted by the smoke test.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// A leading help/version flag describes the PROGRAM, not the default
	// subcommand. Without this, `dockerpull --help` fell through to `serve`
	// and printed Go's raw "Usage of serve:" with exit code 2.
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "-help":
			usage(os.Stdout)
			return exitOK
		case "-v", "--version":
			printVersion()
			return exitOK
		}
	}

	// The subcommand is the first non-flag argument.
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "serve":
		return cmdServe(args)
	case "pull":
		return cmdPull(args)
	case "search":
		return cmdSearch(args)
	case "tags":
		return cmdTags(args)
	case "tasks":
		return cmdTasks(args)
	case "credentials", "cred", "creds":
		return cmdCredentials(args)
	case "login", "logout":
		// Spelled the way users expect; both are `credentials add` / `rm`.
		return cmdCredentials(append([]string{map[string]string{"login": "add", "logout": "rm"}[cmd]}, args...))
	case "version":
		printVersion()
		return exitOK
	case "help", "--help", "-h":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		usage(os.Stderr)
		return exitUsage
	}
}

func printVersion() {
	fmt.Printf("dockerpull %s (commit %s, built %s)\n", version, commit, date)
}

func usage(w *os.File) {
	fmt.Fprint(w, `DockerPull — 拉取 Docker 镜像并导出 docker load 可用的 tar 包

用法:
  dockerpull [serve] [选项]                 启动图形界面服务（默认）
  dockerpull pull   -i <镜像> [选项]        命令行拉取镜像
  dockerpull search -k <关键词> [选项]      搜索镜像
  dockerpull tags   -i <镜像> [选项]        列出镜像的 tag
  dockerpull tasks  <子命令> [id]           管理下载任务
dockerpull login  <host> [-u 用户] [-p 密码] 保存私有仓库凭证（私有镜像免密拉取）
dockerpull logout <host|凭证ID>           删除已保存的凭证
dockerpull credentials <list|add|rm|protection>  凭证管理
  dockerpull version                        显示版本
  dockerpull help                           显示本帮助

serve 选项:
  -port int         HTTP 端口 (默认 8080，也可用 APP_PORT)
  -data-dir string  数据目录 (默认 ./data，也可用 APP_DATA_DIR)
  -dev-proxy string 开发模式：把界面代理到 next dev (例如 http://localhost:3000)

pull 选项:
  -i, --image string       镜像名称，例如 nginx:latest 或 harbor.abc.com/a/b/nginx:1.26.0
  -t, --tag string         镜像 tag（-i 未写 tag 时使用）
  -a, --arch string        架构，默认 amd64（也可写 linux/arm64、arm/v7）
  -o, --output string      输出目录，默认为「数据目录/downloads/镜像_tag_架构」
  -r, --registry string    仓库地址或镜像源 ID（例如 harbor.abc.com、1ms、nju）
      --mirror string      同 -r
      --workers int        并发下载层数，默认 4
  -u, --username string    仓库用户名
  -p, --password string    仓库密码
      --proxy string       代理地址，例如 http://127.0.0.1:7890
      --no-proxy           直连，忽略代理
      --insecure           使用 HTTP 协议连接 registry
      --no-verify-tls      跳过 TLS 证书校验
      --list-arch          仅列出可用架构，不下载
      --ci                 非交互模式：不打印动画进度，适合 CI
  -q, --quiet              静默模式：不询问镜像源/账号/架构
      --debug              打印调试日志

search 选项:
  -k, --keyword string     搜索关键词
      --source string      搜索源 ID（默认 dockerhub，可选 1ms）
      --page int           页码，默认 1
      --page-size int      每页数量，默认 20
      --json               以 JSON 输出
      --no-download        只搜索并查看清单，不下载
  -t, --tag string         选定 tag（默认 latest）
  -a, --arch string        架构（默认 amd64）
  -o, --output string      输出目录（与 --no-download 搭配）

tasks 子命令:
  dockerpull tasks list [--json]
  dockerpull tasks show <id> [--json]
  dockerpull tasks pause|resume|cancel|retry <id>
  dockerpull tasks rm <id> [--files]

示例:
  dockerpull                                    # 打开 http://localhost:8080
  dockerpull pull -i nginx:latest -a arm64
  dockerpull pull -i nginx -o ./out --workers 8 --ci
  dockerpull pull -i nginx --list-arch
  dockerpull pull -i harbor.abc.com/lib/nginx:1.26 -u admin -p secret
  dockerpull search -k redis
  dockerpull tasks list

退出码: 0 成功, 1 失败, 2 用法错误
`)
}

// cmdServe runs the HTTP server (and therefore the GUI).
func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", 0, "override APP_PORT")
	dataDir := fs.String("data-dir", "", "override APP_DATA_DIR")
	devProxy := fs.String("dev-proxy", "", "override APP_DEV_PROXY (e.g. http://localhost:3000)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *showVersion {
		fmt.Printf("dockerpull %s (commit %s, built %s)\n", version, commit, date)
		return exitOK
	}
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = version, commit, date

	cfg := loadConfig(*port, *dataDir, *devProxy)
	config.SetupLogging(cfg.LogLevel)

	// Context: canceled on SIGINT/SIGTERM — the server shuts down
	// gracefully (5s drain) and running downloads park themselves with
	// their resume state intact.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.Boot(cfg)
	if err != nil {
		slog.Error("boot", "err", err)
		return exitFail
	}
	defer application.Close()

	// Shutdown order matters: park the downloads before the store closes,
	// or a task's final write races the close.
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		stop() // restore default signal handling: a second Ctrl+C is fatal
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		application.Tasks.Shutdown(shutdownCtx)
	}()

	runErr := application.Server.Run(ctx)
	<-done
	if runErr != nil {
		slog.Error("server exited", "err", runErr)
		return exitFail
	}
	slog.Info("shutdown complete")
	return exitOK
}

func loadConfig(port int, dataDir, devProxy string) *config.Config {
	cfg := config.Load()
	if port != 0 {
		cfg.Port = port
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	if devProxy != "" {
		cfg.DevProxy = devProxy
	}
	return cfg
}
