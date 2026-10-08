package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/app"
	"github.com/Potterluo/docker-pull-tar/internal/buildinfo"
	"github.com/Potterluo/docker-pull-tar/internal/config"
	"github.com/Potterluo/docker-pull-tar/internal/puller"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// cli.go holds the subcommands that do work: pull, search, tags, tasks.
//
// Every one of them goes through the SAME code as the GUI: `pull` starts a
// task on the task manager (so it lands in the database and shows up in the
// web UI's 下载任务 list with live progress), and `tasks` reads the same
// rows. There is no second implementation of the download engine.

// bootCLI opens the store + task manager without starting a listener.
func bootCLI(dataDir string) (*app.App, error) {
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = version, commit, date
	cfg := loadConfig(0, dataDir, "")
	cfg.LogLevel = "warn" // the CLI owns stdout; keep slog out of it
	config.SetupLogging(cfg.LogLevel)
	return app.Boot(cfg)
}

// cmdPull downloads one image from the command line (F2 + F3).
func cmdPull(args []string) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	var (
		image     = fs.String("i", "", "Docker 镜像名称")
		imageLong = fs.String("image", "", "Docker 镜像名称")
		tag       = fs.String("t", "", "镜像 tag")
		tagLong   = fs.String("tag", "", "镜像 tag")
		arch      = fs.String("a", "amd64", "架构")
		archLong  = fs.String("arch", "", "架构")
		output    = fs.String("o", "", "输出目录")
		outLong   = fs.String("output", "", "输出目录")
		reg       = fs.String("r", "", "自定义仓库地址或镜像源 ID")
		regLong   = fs.String("registry", "", "自定义仓库地址或镜像源 ID")
		mirror    = fs.String("mirror", "", "镜像源（ID 或 host）")
		workers   = fs.Int("workers", 0, "并发下载层数")
		username  = fs.String("u", "", "仓库用户名")
		userLong  = fs.String("username", "", "仓库用户名")
		password  = fs.String("p", "", "仓库密码")
		passLong  = fs.String("password", "", "仓库密码")
		proxy     = fs.String("proxy", "", "代理地址")
		noProxy   = fs.Bool("no-proxy", false, "直连，忽略代理")
		insecure  = fs.Bool("insecure", false, "使用 HTTP 协议连接 registry")
		noVerify  = fs.Bool("no-verify-tls", false, "跳过 TLS 证书校验")
		listArch  = fs.Bool("list-arch", false, "仅列出可用架构")
		ci        = fs.Bool("ci", false, "非交互模式")
		quiet     = fs.Bool("q", false, "静默模式")
		quietLong = fs.Bool("quiet", false, "静默模式")
		debug     = fs.Bool("debug", false, "调试日志")
		dataDir   = fs.String("data-dir", "", "数据目录")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	img := firstNonEmpty(*image, *imageLong)
	imageTag := firstNonEmpty(*tag, *tagLong)
	architecture := firstNonEmpty(*archLong, *arch)
	outputDir := firstNonEmpty(*outLong, *output)
	repository := firstNonEmpty(*regLong, *reg, *mirror)
	user := firstNonEmpty(*userLong, *username)
	pass := firstNonEmpty(*passLong, *password)
	isQuiet := *quiet || *quietLong || *ci

	if *debug {
		os.Setenv("APP_LOG_LEVEL", "debug")
	}

	// Interactive fallback: prompt for the image when it was not passed.
	// In CI that is a hard error — a prompt would hang the pipeline
	// (the legacy tool errored here too).
	if img == "" {
		if *ci {
			fmt.Fprintln(os.Stderr, "错误：CI 模式下必须通过 -i 参数指定镜像名称。")
			return exitUsage
		}
		fmt.Println("🚀 Docker 镜像拉取工具 " + version)
		fmt.Print("请输入 Docker 镜像名称（例如：nginx:latest 或 harbor.abc.com/abc/nginx:1.26.0）：")
		line, _ := readLine()
		img = strings.TrimSpace(line)
		if img == "" {
			fmt.Fprintln(os.Stderr, "错误：镜像名称是必填项。")
			return exitUsage
		}
	}

	// A tag given separately is appended only when the image carries none.
	if imageTag != "" && !hasReferenceSuffix(img) {
		img = img + ":" + imageTag
	}

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := application.Tasks.Config(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 读取配置失败:", err)
		return exitFail
	}

	// --list-arch short-circuits: resolve the platforms and exit 0.
	if *listArch {
		return listArchitectures(ctx, application, repository, img, *insecure, *noVerify, isQuiet)
	}

	// Interactive registry pick when nothing was specified and the user
	// wants to choose. Quiet/CI skip it and use the configured default.
	if repository == "" && !isQuiet {
		repository = promptMirror()
	}
	if repository == "" {
		repository = cfg.DefaultMirror
	}

	if user == "" && pass == "" && !isQuiet {
		user, pass = promptCredentials()
	}

	proxyURL := ""
	switch {
	case *noProxy:
		proxyURL = "-"
	case strings.TrimSpace(*proxy) != "":
		proxyURL = strings.TrimSpace(*proxy)
	}

	spec := tasks.StartSpec{
		Image:       img,
		Registry:    repository,
		Platform:    architecture,
		Workers:     *workers,
		Insecure:    *insecure,
		OutputDir:   outputDir,
		Credentials: registry.Credentials{Username: user, Password: pass},
		ProxyURL:    proxyURL,
	}
	// Certificate verification stays at the stored setting unless the user
	// explicitly opted out. (This is the negative flag: leaving it false
	// means "use the configured behaviour", not "disable verification".)
	if *noVerify {
		spec.SkipVerifyTLS = true
	}

	task, err := application.Tasks.Start(ctx, spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ "+err.Error())
		return exitFail
	}

	// Show the directory the tar will ACTUALLY land in. Printing cfg.OutputDir
	// unconditionally was wrong whenever -o was given: the manager resolves
	// -o first, so the header disagreed with where the file appeared.
	displayDir := outputDir
	if strings.TrimSpace(displayDir) == "" {
		displayDir = cfg.OutputDir
	}
	fmt.Printf("📦 镜像：%s\n📦 架构：%s\n📁 保存目录：%s\n", task.Ref, task.Platform, displayDir)
	if !*ci {
		fmt.Println("📥 开始下载...")
	}

	renderer := newProgressRenderer(*ci)
	result, ok := waitForTask(ctx, application, task.ID, renderer, stop)

	// A Ctrl+C parks the task rather than failing it: the resume ledger
	// stays on disk and `dockerpull tasks resume <id>` continues.
	if ctx.Err() != nil && !ok {
		_ = application.Tasks.Pause(context.Background(), task.ID)
		fmt.Printf("\n⚠️  收到中断信号，正在保存进度并退出...\n")
		fmt.Printf("💡 继续下载：dockerpull tasks resume %s\n", shortID(task.ID))
		return exitFail
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "\n❌ 下载失败：%s\n", result.Error)
		fmt.Fprintf(os.Stderr, "💡 查看详情：dockerpull tasks show %s\n", shortID(task.ID))
		return exitFail
	}

	fmt.Printf("\n✅ 镜像 %s 下载完成！\n", task.Ref)
	fmt.Printf("📦 保存路径：%s\n", result.TarPath)
	fmt.Printf("📊 镜像包大小：%s", puller.FormatBytes(result.TarSize))
	if result.DownloadedBytes > 0 && result.StartedAt.Before(result.FinishedAt) {
		elapsed := result.FinishedAt.Sub(result.StartedAt).Seconds()
		if elapsed > 0 {
			fmt.Printf("　📊 平均下载速度：%s/s", puller.FormatBytes(int64(float64(result.TotalBytes)/elapsed)))
		}
	}
	fmt.Println()
	if !result.StartedAt.IsZero() && !result.FinishedAt.IsZero() {
		fmt.Printf("⏱️  总耗时：%s\n", puller.FormatDuration(result.FinishedAt.Sub(result.StartedAt)))
	}
	fmt.Printf("💡 导入命令：docker load -i %s\n", result.TarPath)
	if result.Error != "" {
		fmt.Printf("⚠️  %s\n", result.Error)
	}
	return exitOK
}

// listArchitectures implements --list-arch: resolve the manifest index and
// print the selectable platforms without downloading anything.
func listArchitectures(ctx context.Context, application *app.App, repository, image string, useHTTP, noVerify, quiet bool) int {
	cfg, err := application.Tasks.Config(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 读取配置失败:", err)
		return exitFail
	}
	if repository == "" {
		repository = cfg.DefaultMirror
	}
	if host, ok := registry.PullHost(repository); ok {
		repository = host
	}
	ref, err := registry.Parse(image, repository)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ "+err.Error())
		return exitFail
	}
	ref.UseHTTP = useHTTP

	opts := puller.Options{
		Ref:         ref,
		WorkDir:     application.Cfg.DataDir,
		ProxyURL:    cfg.ProxyURLForPuller(),
		InsecureTLS: noVerify || !cfg.VerifyTLS,
	}
	info, err := puller.InspectImage(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ "+err.Error())
		return exitFail
	}

	names := registry.PlatformStrings(info.Platforms)
	if len(names) == 0 {
		fmt.Println("📋 该镜像没有可选的架构清单（单架构镜像）")
		return exitOK
	}
	if !quiet {
		fmt.Printf("📦 仓库地址：%s\n📦 镜像：%s\n", ref.Registry, ref.Repository)
	}
	fmt.Printf("📋 当前可用架构：%s\n", strings.Join(names, ", "))
	fmt.Println("✅ --list-arch 模式：已列出所有可用架构，退出")
	return exitOK
}

// cmdSearch searches a source, prints the results, and (unless
// --no-download) downloads the first hit or the one the user picks.
func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var (
		keyword   = fs.String("k", "", "搜索关键词")
		kwLong    = fs.String("keyword", "", "搜索关键词")
		source    = fs.String("source", "", "搜索源 ID")
		page      = fs.Int("page", 1, "页码")
		pageSize  = fs.Int("page-size", 20, "每页数量")
		asJSON    = fs.Bool("json", false, "以 JSON 输出")
		noDl      = fs.Bool("no-download", false, "只搜索，不下载")
		tag       = fs.String("t", "latest", "tag")
		arch      = fs.String("a", "amd64", "架构")
		output    = fs.String("o", "", "输出目录")
		selectIdx = fs.Int("select", 0, "直接下载第 N 个结果（从 1 开始）")
		dataDir   = fs.String("data-dir", "", "数据目录")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	q := firstNonEmpty(*keyword, *kwLong)
	if q == "" {
		fmt.Fprintln(os.Stderr, "错误：关键词不能为空（-k）")
		return exitUsage
	}

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()
	ctx := context.Background()

	cfg, err := application.Tasks.Config(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 读取配置失败:", err)
		return exitFail
	}
	src := firstNonEmpty(*source, cfg.DefaultSearchSource)

	searcher, err := resolveCLISearcher(ctx, application, src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ "+err.Error())
		return exitFail
	}

	res, err := searcher.Search(ctx, q, *page, *pageSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 搜索失败:", err)
		return exitFail
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"source":   searcher.ID(),
			"keyword":  q,
			"total":    res.Total,
			"page":     res.Page,
			"pageSize": res.PageSize,
			"results":  res.Results,
		})
		if *noDl || *selectIdx == 0 {
			return exitOK
		}
	}

	if len(res.Results) == 0 {
		fmt.Println("没有搜索到结果")
		return exitOK
	}

	// A search result is a NAME ("library/nginx"), not a location. Pulling it
	// bare reaches registry-1.docker.io whatever source produced it — so say
	// out loud where the download will come from, and pass that host to pull.
	pullHost := registry.PullHostForSearchSource(searcher.ID())

	if !*asJSON {
		fmt.Printf("🔍 搜索源：%s　关键词：%s　共 %d 个结果\n", searcher.Name(), q, res.Total)
		if pullHost != "" {
			fmt.Printf("⬇️  下载源：%s\n", pullHost)
		} else {
			fmt.Printf("⬇️  下载源：默认镜像源（可在「设置」中修改）\n")
		}
		fmt.Println()
		fmt.Printf("%-4s %-34s %-8s %-12s %-12s %s\n", "序号", "镜像名称", "Stars", "Pulls", "更新日期", "描述")
		for i, r := range res.Results {
			fmt.Printf("%-4d %-34s %-8d %-12d %-12s %s\n",
				i+1, truncateStr(r.Name, 34), r.Stars, r.Pulls, r.Updated, truncateStr(r.Description, 40))
		}
		fmt.Println()
	}

	if *noDl {
		fmt.Println("🧪 --no-download 已开启：仅列出搜索结果，未下载。")
		return exitOK
	}

	// Pick the target: an explicit --select, a single result, or a prompt.
	choose := *selectIdx
	if choose == 0 {
		if len(res.Results) == 1 {
			choose = 1
		} else if !*asJSON {
			fmt.Printf("请输入要下载的序号（1-%d，回车取消）：", len(res.Results))
			line, _ := readLine()
			line = strings.TrimSpace(line)
			if line == "" {
				fmt.Println("⚠️  用户取消操作。")
				return exitOK
			}
			n, convErr := strconv.Atoi(line)
			if convErr != nil || n < 1 || n > len(res.Results) {
				fmt.Fprintln(os.Stderr, "序号超出范围")
				return exitUsage
			}
			choose = n
		} else {
			return exitUsage
		}
	}
	if choose < 1 || choose > len(res.Results) {
		fmt.Fprintln(os.Stderr, "序号超出范围")
		return exitUsage
	}

	target := res.Results[choose-1]
	image := target.Name
	if *tag != "" && !hasReferenceSuffix(image) {
		image = image + ":" + *tag
	}

	// Reuse the pull path end to end, carrying the source's host so a 1ms
	// search actually downloads through 1ms.
	pullArgs := []string{"-i", image, "-a", *arch, "--data-dir", *dataDir}
	if pullHost != "" {
		pullArgs = append(pullArgs, "--mirror", pullHost)
	}
	if *output != "" {
		pullArgs = append(pullArgs, "-o", *output)
	}
	pullArgs = append(pullArgs, "--ci")
	return cmdPull(pullArgs)
}

// cmdTags lists a repository's tags.
func cmdTags(args []string) int {
	fs := flag.NewFlagSet("tags", flag.ContinueOnError)
	var (
		image    = fs.String("i", "", "镜像名称")
		imgLong  = fs.String("image", "", "镜像名称")
		source   = fs.String("source", "", "搜索源 ID（仅在 registry 不支持 tags/list 时兜底）")
		mirror   = fs.String("mirror", "", "下载源（镜像源 ID 或 host）")
		mirrorM  = fs.String("m", "", "下载源（镜像源 ID 或 host）")
		insecure = fs.Bool("insecure", false, "使用 HTTP 协议连接 registry")
		noVerify = fs.Bool("no-verify-tls", false, "跳过 TLS 证书校验")
		asJSON   = fs.Bool("json", false, "以 JSON 输出")
		dataDir  = fs.String("data-dir", "", "数据目录")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	img := firstNonEmpty(*image, *imgLong)
	if img == "" {
		fmt.Fprintln(os.Stderr, "错误：必须通过 -i 指定镜像名称。")
		return exitUsage
	}

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()
	ctx := context.Background()

	cfg, err := application.Tasks.Config(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 读取配置失败:", err)
		return exitFail
	}
	custom := firstNonEmpty(*mirror, *mirrorM, cfg.DefaultMirror)
	if host, ok := registry.PullHost(custom); ok {
		custom = host
	} else {
		fmt.Fprintf(os.Stderr, "❌ 无法识别的下载源 %q：可用 ID（1ms/mcr/quay/ghcr…）或 host（registry.example.com:5000）\n", custom)
		return exitUsage
	}
	ref, err := registry.Parse(img, custom)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ "+err.Error())
		return exitFail
	}
	ref.UseHTTP = *insecure

	// Tags come from the registry the download would use, because that is the
	// only list guaranteed to be pullable. The search API is a fallback for a
	// registry without tags/list — it is frequently a DIFFERENT registry, so
	// taking its answer first offered tags the pull could not have. It does
	// carry dates and architectures, which is why it is still worth asking.
	tagsFrom := ""
	var tags []registry.TagInfo
	opts := puller.Options{
		Ref:         ref,
		WorkDir:     application.Cfg.DataDir,
		ProxyURL:    cfg.ProxyURLForPuller(),
		InsecureTLS: *noVerify || !cfg.VerifyTLS,
	}
	// ListTags first: it needs only the repository, so it still works when the
	// ref's own tag is missing (InspectImage would fail on the manifest).
	if names, host, lerr := puller.ListTags(ctx, opts); lerr == nil && len(names) > 0 {
		tagsFrom = host
		for _, name := range names {
			tags = append(tags, registry.TagInfo{Name: name})
		}
		return emitTags(ref, tagsFrom, tags, *asJSON)
	}

	info, ierr := puller.InspectImage(ctx, opts)
	switch {
	case ierr == nil && len(info.Tags) > 0:
		tagsFrom = info.TagsFrom
		for _, name := range info.Tags {
			tags = append(tags, registry.TagInfo{Name: name})
		}
	default:
		src := firstNonEmpty(*source, cfg.DefaultSearchSource)
		searcher, serr := resolveCLISearcher(ctx, application, src)
		// A searcher may only answer for its OWN namespace: Docker Hub's API
		// describing a ghcr.io repository returns a different tag list (42 vs
		// 38 for linuxcontainers/alpine), i.e. tags this pull cannot have.
		if serr == nil && !registry.SearcherServesRegistry(ref.Registry, searcher.ID()) {
			serr = fmt.Errorf("搜索源 %s 与 %s 不同源，不能用它的标签", searcher.ID(), ref.Registry)
		}
		if serr != nil {
			if ierr != nil {
				fmt.Fprintln(os.Stderr, "❌ "+ierr.Error())
				return exitFail
			}
			fmt.Fprintln(os.Stderr, "❌ "+serr.Error())
			return exitFail
		}
		found, terr := searcher.Tags(ctx, ref.Repository)
		if terr != nil {
			if ierr != nil {
				fmt.Fprintln(os.Stderr, "❌ "+ierr.Error())
				return exitFail
			}
			fmt.Fprintln(os.Stderr, "❌ 获取 tag 失败:", terr)
			return exitFail
		}
		tags = found
		tagsFrom = searcher.ID()
	}

	return emitTags(ref, tagsFrom, tags, *asJSON)
}

// emitTags renders a tag listing, with its provenance.
func emitTags(ref registry.Ref, tagsFrom string, tags []registry.TagInfo, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"repository": ref.Repository,
			"registry":   ref.Registry,
			"tagsFrom":   tagsFrom,
			"tags":       tags,
		})
		return exitOK
	}
	if len(tags) == 0 {
		fmt.Println("没有查询到 tag")
		return exitOK
	}
	fmt.Printf("🏷️  %s 的可用 tag（%d 个）", ref.Repository, len(tags))
	if tagsFrom != "" {
		fmt.Printf("　来源：%s", tagsFrom)
	}
	fmt.Println("：")
	for _, t := range tags {
		archs := registry.PlatformStrings(t.Platforms)
		suffix := ""
		if len(archs) > 0 {
			suffix = "  [" + strings.Join(archs, ",") + "]"
		}
		fmt.Printf("  %-28s %s%s\n", t.Name, t.Updated, suffix)
	}
	return exitOK
}

// resolveCLISearcher maps a source id (built-in id, or a stored row id)
// onto a live searcher, mirroring what the HTTP layer does.
func resolveCLISearcher(ctx context.Context, application *app.App, id string) (registry.Searcher, error) {
	sources := registry.SearchSources(nil)
	if searcher, err := registry.PickSearcher(sources, id); err == nil {
		return searcher, nil
	}
	row, err := application.Store.GetSource(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			return nil, fmt.Errorf("未知的搜索源 %q（可用: dockerhub, 1ms）", id)
		}
		return nil, err
	}
	for _, builtin := range registry.BuiltinSearchSources {
		if strings.EqualFold(row.URL, builtin.URL) {
			return sources[builtin.ID], nil
		}
	}
	if strings.Contains(row.URL, "1ms") {
		return registry.NewOneMSSearcher(nil, row.URL), nil
	}
	if strings.Contains(row.URL, "dockerhub") || strings.Contains(row.URL, "hub.docker.com") {
		return registry.NewDockerHubSearcher(nil, row.URL), nil
	}
	return nil, fmt.Errorf("无法识别的搜索源 %q", row.Name)
}

// cmdTasks manages existing tasks.
func cmdTasks(args []string) int {
	// `dockerpull tasks` and `dockerpull tasks --data-dir X` mean "list".
	// Taking args[0] as the verb unconditionally made the FLAG look like an
	// unknown subcommand ("未知子命令 --data-dir"), which is a confusing way to
	// say "you left out the verb" — and listing is the only useful default.
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	} else if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stderr, "用法: dockerpull tasks [list|show|pause|resume|cancel|retry|rm] [id] [选项]")
		return exitUsage
	}
	rest := args

	fs := flag.NewFlagSet("tasks "+sub, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	removeFiles := fs.Bool("files", false, "同时删除下载缓存与 tar 文件")
	dataDir := fs.String("data-dir", "", "数据目录")

	// Go's flag package stops parsing at the first non-flag argument, so
	// `tasks show <id> --json` would silently ignore --json and every later
	// flag. Reorder first: flags (with the values of value-taking flags) to
	// the front, positionals after — the same trick cmd/generator uses.
	flags, positional := splitFlags(rest, map[string]bool{"data-dir": true})
	if err := fs.Parse(flags); err != nil {
		return exitUsage
	}
	positional = append(positional, fs.Args()...)

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()
	ctx := context.Background()

	switch sub {
	case "list":
		list, err := application.Tasks.List(ctx, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "❌ 读取任务失败:", err)
			return exitFail
		}
		if *asJSON {
			return emitJSON(map[string]any{"tasks": list})
		}
		if len(list) == 0 {
			fmt.Println("暂无下载任务")
			return exitOK
		}
		fmt.Printf("%-10s %-38s %-12s %-10s %-18s %s\n", "ID", "镜像", "架构", "状态", "进度", "创建时间")
		for _, t := range list {
			fmt.Printf("%-10s %-38s %-12s %-10s %-18s %s\n",
				shortID(t.ID), truncateStr(t.Ref, 38), t.Platform, statusLabel(t.Status),
				progressLabel(t.Task), t.CreatedAt.Local().Format("2006-01-02 15:04:05"))
		}
		return exitOK

	case "show":
		t, err := requireTaskID(application, ctx, positional)
		if err != nil {
			fmt.Fprintln(os.Stderr, "❌ "+err.Error())
			return taskErrorExit(err)
		}
		if *asJSON {
			return emitJSON(map[string]any{"task": t})
		}
		printTaskDetail(t)
		return exitOK

	case "pause", "resume", "cancel", "retry", "rm":
		id, err := resolveTaskIDArg(application, ctx, positional)
		if err != nil {
			fmt.Fprintln(os.Stderr, "❌ "+err.Error())
			return taskErrorExit(err)
		}
		switch sub {
		case "pause":
			err = application.Tasks.Pause(ctx, id)
		case "resume":
			err = application.Tasks.Resume(ctx, id)
		case "cancel":
			err = application.Tasks.Cancel(ctx, id)
		case "retry":
			err = application.Tasks.Retry(ctx, id)
		case "rm":
			err = application.Tasks.Delete(ctx, id, *removeFiles)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "❌ "+err.Error())
			return exitFail
		}
		fmt.Printf("✅ 已执行 %s：%s\n", sub, shortID(id))
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", sub)
		return exitUsage
	}
}

// --- task waiting + rendering -------------------------------------------

// waitForTask polls the task row until it reaches a terminal state,
// rendering progress on the way. Polling the store (rather than consuming
// the puller's own sink) is what keeps the CLI and the GUI on one code
// path: the console sees exactly the bytes the web UI sees.
func waitForTask(ctx context.Context, application *app.App, id string, r *progressRenderer, stop func()) (store.Task, bool) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	var last store.Task
	for {
		t, err := application.Tasks.Get(ctx, id)
		if err != nil {
			// The context was canceled: report the last known state.
			if ctx.Err() != nil {
				return last, false
			}
			return last, false
		}
		last = t.Task
		r.Render(t.Task, t.Layers)

		switch t.Status {
		case tasks.StatusSucceeded:
			return t.Task, true
		case tasks.StatusFailed, tasks.StatusCanceled:
			return t.Task, false
		case tasks.StatusPaused:
			// Paused without us asking: a shutdown parked it.
			return t.Task, false
		}

		select {
		case <-ctx.Done():
			return last, false
		case <-ticker.C:
		}
	}
}

// progressRenderer draws the per-layer progress panel.
//
// In CI mode it prints one line per finished layer instead (no ANSI
// redraws, no spinners) because `\033[F` in a CI log is noise.
type progressRenderer struct {
	ci       bool
	lines    int
	reported map[string]bool
}

func newProgressRenderer(ci bool) *progressRenderer {
	return &progressRenderer{ci: ci, reported: make(map[string]bool)}
}

// Render draws the current state.
func (r *progressRenderer) Render(t store.Task, layers []store.TaskLayer) {
	if r.ci {
		for _, l := range layers {
			if l.Status != "completed" && l.Status != "skipped" {
				continue
			}
			if r.reported[l.ID] {
				continue
			}
			r.reported[l.ID] = true
			fmt.Printf("✅ %-12s 完成 (%s)\n", l.Name, puller.FormatBytes(l.Size))
		}
		return
	}

	sort.SliceStable(layers, func(i, j int) bool { return layers[i].Position < layers[j].Position })

	var b strings.Builder
	b.WriteString("🐳 Docker 镜像下载中\n")
	for i, l := range layers {
		icon, width := "⏳", 0.0
		switch l.Status {
		case "completed", "skipped":
			icon, width = "✅", 100
		case "downloading":
			icon = "⬇️"
		case "failed":
			icon = "❌"
		}
		if l.Status == "downloading" && l.Size > 0 {
			width = float64(l.Downloaded) / float64(l.Size) * 100
			if width > 100 {
				width = 100
			}
		}
		bar := bar(width, 24)
		size := "?"
		if l.Size > 0 {
			size = puller.FormatBytes(l.Size)
		}
		fmt.Fprintf(&b, "  %s (%d/%d) %-14s |%s| %5.1f%% %10s/%s\n",
			icon, i+1, len(layers), truncateStr(l.Name, 14), bar, width,
			puller.FormatBytes(l.Downloaded), size)
	}
	if t.TotalBytes > 0 {
		pct := float64(t.DownloadedBytes) / float64(t.TotalBytes) * 100
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(&b, "  总进度 %s %.1f%%", bar(pct, 24), pct)
	}
	if t.Speed > 0 {
		fmt.Fprintf(&b, "　📊 速度 %s/s", puller.FormatBytes(int64(t.Speed)))
	}
	b.WriteString("\n")

	r.redraw(b.String())
}

// redraw replaces the previous panel in place.
func (r *progressRenderer) redraw(text string) {
	if r.lines > 0 {
		fmt.Printf("\033[%dF\033[J", r.lines)
	}
	fmt.Print(text)
	r.lines = strings.Count(text, "\n")
}

func bar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100 * float64(width))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// --- prompts ------------------------------------------------------------

// promptMirror shows the built-in mirror menu. An empty answer keeps the
// configured default, which is what the legacy tool did too.
func promptMirror() string {
	fmt.Println("请选择镜像站：")
	for i, m := range registry.BuiltinMirrors {
		fmt.Printf("  %2d. %s (%s)\n", i+1, m.Name, m.Host)
	}
	fmt.Printf("请输入序号（回车使用默认 %s）：", registry.DefaultMirror().Name)
	line, _ := readLine()
	line = strings.TrimSpace(line)
	if line == "" {
		return registry.DefaultMirror().Host
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(registry.BuiltinMirrors) {
		fmt.Println("输入无效，使用默认镜像站")
		return registry.DefaultMirror().Host
	}
	m := registry.BuiltinMirrors[n-1]
	fmt.Printf("✅ 已选择镜像站：%s\n", m.Name)
	return m.Host
}

// promptCredentials asks for registry credentials. Blank input means
// "anonymous", which is the common case.
func promptCredentials() (string, string) {
	fmt.Print("请输入镜像仓库用户名（回车跳过，匿名拉取）：")
	user, _ := readLine()
	user = strings.TrimSpace(user)
	if user == "" {
		return "", ""
	}
	fmt.Print("请输入镜像仓库密码：")
	pass, _ := readLine()
	return user, strings.TrimSpace(pass)
}

// readLine reads one line from stdin, tolerating a closed stdin (a pipe
// that ended) by returning an empty string rather than hanging or panicking.
func readLine() (string, error) {
	var line string
	_, err := fmt.Scanln(&line)
	return line, err
}

// --- small helpers ------------------------------------------------------

// errMissingTaskID separates "you forgot the id" (a usage error, exit 2) from
// "there is no such task" (a runtime failure, exit 1). Reporting the second as
// a usage error told scripts the wrong thing.
var errMissingTaskID = errors.New("缺少任务 ID")

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// splitFlags separates flag tokens from positional arguments, regardless of
// the order they were typed in.
//
// This exists because Go's `flag` package stops parsing at the first
// non-flag argument: `tasks show <id> --json` would otherwise parse `<id>`
// as the start of the positional list and silently drop `--json`,
// `--files` and `--data-dir`. Silent flag loss is worse than a usage error,
// so the arguments are reordered before parsing.
//
// valueFlags names the flags that consume a following value (`--data-dir x`);
// boolean flags must NOT be listed, or the next token would be swallowed.
// `--name=value` form needs no special handling.
func splitFlags(args []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue // --name=value carries its own value
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

// hasReferenceSuffix reports whether img already carries a tag or a digest,
// so a separately supplied --tag is not appended.
//
// Two cases matter:
//
//   - a digest (`repo@sha256:...`) already pins the exact manifest, and
//     appending ":latest" would produce "repo@sha256:abc:latest", which is
//     not a valid reference at all;
//   - a colon AFTER the last '/' is a tag; a colon before it belongs to a
//     registry port (`harbor.abc.com:5000/a/b`), which is not a tag.
func hasReferenceSuffix(img string) bool {
	if strings.ContainsRune(img, '@') {
		return true
	}
	return strings.LastIndex(img, ":") > strings.LastIndex(img, "/")
}

func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// shortID renders a task id for display. The stored id is 32 hex chars;
// the first 10 are plenty to address it interactively.
func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:10]
}

// resolveTaskIDArg accepts either a full id or the short prefix the list
// view prints.
func resolveTaskIDArg(application *app.App, ctx context.Context, positional []string) (string, error) {
	if len(positional) == 0 {
		return "", errMissingTaskID
	}
	arg := strings.TrimSpace(positional[0])
	if arg == "" {
		return "", errMissingTaskID
	}
	if _, err := application.Store.GetTask(ctx, arg); err == nil {
		return arg, nil
	}
	// Fall back to a unique prefix match.
	list, err := application.Store.ListTasks(ctx, 0)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, t := range list {
		if strings.HasPrefix(t.ID, arg) {
			matches = append(matches, t.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("找不到任务 %q", arg)
	default:
		return "", fmt.Errorf("任务 ID %q 有歧义（匹配 %d 个）", arg, len(matches))
	}
}

// taskErrorExit maps a task-resolution error onto an exit code: a missing
// argument is a usage error, an unknown or ambiguous id is a failure.
func taskErrorExit(err error) int {
	if errors.Is(err, errMissingTaskID) {
		return exitUsage
	}
	return exitFail
}

func requireTaskID(application *app.App, ctx context.Context, positional []string) (*tasks.Task, error) {
	id, err := resolveTaskIDArg(application, ctx, positional)
	if err != nil {
		return nil, err
	}
	return application.Tasks.Get(ctx, id)
}

func printTaskDetail(t *tasks.Task) {
	fmt.Printf("ID：%s\n镜像：%s\n架构：%s\n状态：%s\n", t.ID, t.Ref, t.Platform, statusLabel(t.Status))
	fmt.Printf("进度：%s\n", progressLabel(t.Task))
	if t.Error != "" {
		fmt.Printf("错误：%s\n", t.Error)
	}
	if t.TarPath != "" {
		fmt.Printf("文件：%s (%s)\n", t.TarPath, puller.FormatBytes(t.TarSize))
		fmt.Printf("导入：docker load -i %s\n", t.TarPath)
	}
	fmt.Printf("工作目录：%s\n创建时间：%s\n", t.WorkDir, t.CreatedAt.Local().Format(time.RFC3339))
	if len(t.Layers) > 0 {
		fmt.Println("层：")
		sort.SliceStable(t.Layers, func(i, j int) bool { return t.Layers[i].Position < t.Layers[j].Position })
		for i, l := range t.Layers {
			fmt.Printf("  %2d. %-14s %-12s %10s/%-10s %s\n",
				i+1, l.Name, l.Status, puller.FormatBytes(l.Downloaded),
				puller.FormatBytes(l.Size), l.Error)
		}
	}
}

func progressLabel(t store.Task) string {
	if t.TotalBytes <= 0 {
		return puller.FormatBytes(t.DownloadedBytes)
	}
	pct := float64(t.DownloadedBytes) / float64(t.TotalBytes) * 100
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("%.1f%% (%s/%s)", pct, puller.FormatBytes(t.DownloadedBytes), puller.FormatBytes(t.TotalBytes))
}

// statusLabel renders a task status in the language the UI uses.
func statusLabel(status string) string {
	switch status {
	case tasks.StatusPending:
		return "等待中"
	case tasks.StatusRunning:
		return "下载中"
	case tasks.StatusPaused:
		return "已暂停"
	case tasks.StatusSucceeded:
		return "已完成"
	case tasks.StatusFailed:
		return "失败"
	case tasks.StatusCanceled:
		return "已取消"
	default:
		return status
	}
}

func emitJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "❌ 输出 JSON 失败:", err)
		return exitFail
	}
	return exitOK
}
