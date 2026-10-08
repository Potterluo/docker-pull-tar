package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// cli_credentials.go holds `login`, `logout` and `credentials`.
//
// Stored logins are what make private images painless: save one with
// `dockerpull login ghcr.io` and every later `dockerpull pull ghcr.io/owner/x`
// uses it with no flags. The secret is encrypted at rest (DPAPI on Windows) and
// is never printed, logged, or returned by any API.

// cmdCredentials dispatches the credential management subcommands:
//
//	dockerpull credentials list [--json]
//	dockerpull credentials add <host> [-u user] [-p pass] [--kind basic|token]
//	dockerpull credentials rm <host|id>
//	dockerpull credentials protection
func cmdCredentials(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: dockerpull credentials <list|add|rm|protection> [host|id] [选项]")
		fmt.Fprintln(os.Stderr, "  或: dockerpull login <host> -u <用户名> -p <密码/令牌>")
		fmt.Fprintln(os.Stderr, "  或: dockerpull logout <host>")
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return credentialsList(rest)
	case "add", "set":
		return credentialsAdd(rest)
	case "rm", "remove", "delete":
		return credentialsRemove(rest)
	case "protection", "info":
		return credentialsInfo(rest)
	default:
		// `credentials ghcr` reads naturally as "show me this host"; anything
		// else is a typo worth reporting rather than treating as a filter.
		if !strings.HasPrefix(sub, "-") {
			fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", sub)
		}
		fmt.Fprintln(os.Stderr, "用法: dockerpull credentials <list|add|rm|protection> [host|id] [选项]")
		return exitUsage
	}
}

// cmdLogin is `credentials add`, spelled the way users expect.
func cmdLogin(args []string) int { return credentialsAdd(args) }

// cmdLogout is `credentials rm`, spelled the way users expect.
func cmdLogout(args []string) int { return credentialsRemove(args) }

func credentialsList(args []string) int {
	fs := flag.NewFlagSet("credentials list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	dataDir := fs.String("data-dir", "", "数据目录")

	// See cmdTasks: Go's flag package stops at the first positional, so flags
	// are reordered to the front before parsing.
	flags, positional := splitFlags(args, map[string]bool{"data-dir": true})
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

	list, err := application.Tasks.ListCredentials(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 读取凭证失败:", err)
		return exitFail
	}
	filter := ""
	if len(positional) > 0 {
		filter = strings.ToLower(strings.TrimSpace(positional[0]))
	}
	if filter != "" {
		kept := make([]tasks.CredentialView, 0, len(list))
		for _, c := range list {
			if strings.Contains(strings.ToLower(c.Host), filter) {
				kept = append(kept, c)
			}
		}
		list = kept
	}

	if *asJSON {
		return emitJSON(map[string]any{
			"credentials": list,
			"protection":  application.Tasks.ProtectorLabel(),
		})
	}

	if len(list) == 0 {
		fmt.Println("（未保存任何仓库凭证）")
		fmt.Println("提示: dockerpull login <registry-host> -u <用户名> -p <密码/令牌>")
		return exitOK
	}
	fmt.Printf("已保存 %d 条凭证　加密方式: %s\n\n", len(list), application.Tasks.ProtectorLabel())
	fmt.Printf("%-28s %-20s %-7s %s\n", "仓库", "用户名", "类型", "备注")
	for _, c := range list {
		fmt.Printf("%-28s %-20s %-7s %s\n",
			truncateStr(c.Host, 27), truncateStr(orDash(c.Username), 19), c.Kind, c.Note)
	}
	// The ids are what `rm` accepts, so they go last rather than interrupting
	// the table.
	fmt.Println()
	for _, c := range list {
		fmt.Printf("  %s  %s\n", c.ID, c.Host)
	}
	return exitOK
}

func credentialsAdd(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	user := fs.String("u", "", "仓库用户名（token 类型可留空）")
	userLong := fs.String("user", "", "仓库用户名")
	pass := fs.String("p", "", "仓库密码或访问令牌")
	passLong := fs.String("password", "", "仓库密码或访问令牌")
	kind := fs.String("kind", tasks.CredentialBasic, "凭证类型: basic | token")
	note := fs.String("note", "", "备注")
	dataDir := fs.String("data-dir", "", "数据目录")

	flags, positional := splitFlags(args, map[string]bool{
		"user": true, "u": true, "password": true, "p": true,
		"kind": true, "note": true, "data-dir": true,
	})
	if err := fs.Parse(flags); err != nil {
		return exitUsage
	}
	positional = append(positional, fs.Args()...)

	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "用法: dockerpull login <registry-host> [-u 用户名] [-p 密码/令牌]")
		fmt.Fprintln(os.Stderr, "例如: dockerpull login ghcr.io -u myuser -p ghp_xxxxxx")
		fmt.Fprintln(os.Stderr, "      dockerpull login harbor.internal:5000 -u admin -p secret")
		return exitUsage
	}
	rawHost := positional[0]
	host := registry.NormalizeAuthHost(rawHost)
	if host != strings.TrimSpace(rawHost) {
		fmt.Printf("📦 仓库地址：%s  →  %s\n", strings.TrimSpace(rawHost), host)
	}

	username := firstNonEmpty(*userLong, *user)
	secret := firstNonEmpty(*passLong, *pass)
	// Prompt instead of silently saving nothing. fmt.Scanln echoes, so this is
	// not a hidden prompt; -p exists for scripts and for piping.
	if secret == "" {
		fmt.Print("请输入密码或访问令牌: ")
		line, _ := readLine()
		secret = strings.TrimSpace(line)
	}
	if secret == "" {
		fmt.Fprintln(os.Stderr, "❌ 密码/令牌不能为空")
		return exitUsage
	}
	if username == "" && *kind == tasks.CredentialBasic {
		fmt.Print("请输入用户名（回车跳过，将按 token 类型保存）: ")
		line, _ := readLine()
		username = strings.TrimSpace(line)
		if username == "" {
			*kind = tasks.CredentialToken
		}
	}

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()

	saved, err := application.Tasks.SaveCredential(context.Background(), host, username, secret, *kind, *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 保存凭证失败:", err)
		return exitFail
	}
	fmt.Printf("✅ 已保存 %s 的凭证（用户 %s，类型 %s）\n", saved.Host, orDash(saved.Username), saved.Kind)
	fmt.Printf("🔐 加密方式：%s\n", application.Tasks.ProtectorLabel())
	fmt.Printf("💡 现在可以直接拉取：dockerpull pull -i %s/<命名空间>/<镜像>:<tag>\n", saved.Host)
	return exitOK
}

func credentialsRemove(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "数据目录")

	flags, positional := splitFlags(args, map[string]bool{"data-dir": true})
	if err := fs.Parse(flags); err != nil {
		return exitUsage
	}
	positional = append(positional, fs.Args()...)

	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "用法: dockerpull logout <registry-host|凭证ID>")
		return exitUsage
	}

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()

	ctx := context.Background()
	target := strings.TrimSpace(positional[0])
	// Accept the id first (that is what `credentials list` prints), then a host.
	if strings.HasPrefix(target, "cred_") {
		if err := application.Tasks.DeleteCredential(ctx, target); err != nil {
			if err == store.ErrNotFound {
				fmt.Fprintf(os.Stderr, "❌ 凭证 %s 不存在\n", target)
				return exitFail
			}
			fmt.Fprintln(os.Stderr, "❌ 删除凭证失败:", err)
			return exitFail
		}
		fmt.Printf("✅ 已删除凭证 %s\n", target)
		return exitOK
	}

	host := registry.NormalizeAuthHost(target)
	if err := application.Tasks.DeleteCredentialByHost(ctx, host); err != nil {
		if err == store.ErrNotFound {
			fmt.Fprintf(os.Stderr, "❌ %s 没有保存凭证\n", host)
			return exitFail
		}
		fmt.Fprintln(os.Stderr, "❌ 删除凭证失败:", err)
		return exitFail
	}
	fmt.Printf("✅ 已删除 %s 的凭证\n", host)
	return exitOK
}

// credentialsInfo explains how secrets are protected here — and what that does
// NOT cover, so nobody assumes more than the platform provides.
func credentialsInfo(args []string) int {
	fs := flag.NewFlagSet("credentials protection", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "数据目录")

	flags, _ := splitFlags(args, map[string]bool{"data-dir": true})
	if err := fs.Parse(flags); err != nil {
		return exitUsage
	}

	application, err := bootCLI(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 初始化失败:", err)
		return exitFail
	}
	defer application.Close()

	fmt.Printf("🔐 凭证加密方式：%s\n\n", application.Tasks.ProtectorLabel())
	fmt.Println("Windows 使用 DPAPI：密钥由系统按当前用户账户托管，不落盘，")
	fmt.Println("因此只有同一个 Windows 账户能解密——把 app.db 拷到别的机器/账户上是打不开的。")
	fmt.Println("其它平台使用数据目录下的 secret.key（AES-256-GCM），能防明文泄露，")
	fmt.Println("但不防同一用户账户下的其它进程。")
	fmt.Println()
	fmt.Println("无论哪种：密码都不会以明文写进数据库、日志或任何 API 响应。")
	return exitOK
}

// --- small helpers ------------------------------------------------------

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
