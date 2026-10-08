# DockerPull 功能梳理（FEATURES）

本文档是这次重写的**功能底稿**：先清算旧 Python 版本"到底有什么、哪里坏了"，再按新契约
（[docs/CONTRACT.md](./CONTRACT.md)）列出新架构的功能面、断点续传的准确语义、API/CLI/配置一览，
最后给出旧 → 新的迁移对照表和诚实的待办清单。

- 新实现的事实来源：`docs/CONTRACT.md`（冻结接口契约）。
- 旧实现的事实来源：`../legacy/`（3 份 Python 实现，见下）。
- 本文只描述 CONTRACT 已经写死的东西，不发明功能。

---

## 1. 一句话定位

**在内网 / 离线 / 没有 Docker 环境的地方，把 Docker registry 上的镜像（可按架构挑选）拉下来，
导出成 `docker load -i` 可以直接导入的 `.tar` 包。**

用户故事：外网机器上 `dockerpull pull -i nginx:1.26 -a amd64`（或打开 GUI 点两下）→ 得到
`nginx_1.26_amd64.tar` → U 盘拷进内网 → `docker load -i nginx_1.26_amd64.tar` → `docker run`。

它不是 Docker 的替代品，也不是 registry 代理：没有守护进程、没有容器运行时依赖、单文件即可运行。

---

## 2. 原有功能全景（去重后）

旧仓库里的"三个实现"其实是同一套引擎的三份拷贝：

| 文件 | 行数 | 自称版本 | 定位 |
|---|---:|---|---|
| `../legacy/docker_image_puller.py` | 1511 | `v1.9.0` | 全功能 CLI，唯一"真正的主线" |
| `../legacy/docker_image_puller_1ms.py` | 1369 | `v1.0.0-1ms` | 主线的 fork，加了 1ms 关键词搜索 CLI |
| `../legacy/app.py` | 707 | 无（内嵌） | Gradio GUI，把同一套引擎**第三次**重写 |

合计约 3600 行 Python。其中 `docker_image_puller_1ms.py` 有约 700 行与主线近乎逐行重复；
`app.py` 不复用任何模块，把"认证 → 取清单 → 选架构 → 下载 → 校验 → 打包"整条链路又写了一遍。

### 2.1 能力矩阵

图例：✅ 完整可用 · ⚠️ 部分/有缺陷 · ❌ 不存在。

| 能力 | 全功能 CLI<br>`docker_image_puller.py` | 1ms fork<br>`docker_image_puller_1ms.py` | Gradio GUI<br>`app.py` | 说明 |
|---|:--:|:--:|:--:|---|
| 镜像名解析 | ✅ | ⚠️ | ⚠️ | CLI 支持 `nginx` / `nginx:tag` / `harbor.example.com/a/b:tag` + `-r`；fork 与 GUI 的仓库名只能从搜索结果拼 `namespace/name`，registry 由下拉框决定。**三份都不支持 `repo@sha256:...` 摘要引用**（`nginx@sha256:abc` 会被解析成 image=`nginx@sha256`、tag=`abc`） |
| 多架构选择 | ✅ | ⚠️ | ⚠️ | CLI 从清单 `platform` / bashbrew 注解取架构并做别名模糊匹配、唯一架构自动选择；fork 有别名匹配但交互分支弱；GUI 的架构下拉框候选是**写死的** `amd64/arm64/arm/v7/s390x/ppc64le`，不是从镜像清单读出来的 |
| registry 认证（token / Basic） | ✅ | ⚠️ | ⚠️ | CLI 的 `WWW-Authenticate` 解析健壮，Bearer + Basic 兜底；fork 与 GUI 用 `split('"')[1]` / `[3]` 取值，遇到 Basic challenge 直接 IndexError。fork 的 `-u/-p` 没有入口（匿名），GUI 根本没有账号输入框 |
| 断点续传 | ✅ | ⚠️ | ⚠️ | 三者都发 `Range`；**只有 CLI 判别 206/200**（服务器忽略 Range 返回 200 时丢弃半截文件重下）。fork 与 GUI 以 `mode='ab'` 追加，会把全文拼到半截文件上（见 §3） |
| 分片下载 | ✅ | ✅ | ⚠️ | `>50MB` 走分片 + 线程池（`--workers`）；GUI 版三份里最简，统计口径也不同 |
| SHA256 校验 | ✅ | ✅ | ❌ | GUI 的下载函数收下了 `expected_digest` 参数，函数体内**从头到尾没有做过一次哈希比对** |
| 失败重试 | ✅ | ⚠️ | ⚠️ | CLI：认证 3 次 / 清单 3 次 / 下载 10 次 + 指数退避；fork 依赖 `urllib3.Retry` + 自己的循环；GUI 只有 `Retry(total=3)` 和固定 `sleep(1)` |
| 代理 / SSL | ⚠️ | ❌ | ✅ | CLI 有 `--proxy` / `--no-proxy`；fork 只有环境变量代理；**只有 GUI 的"开启 SSL 证书验证"勾选框真正生效**——CLI/fork 的每个请求都写死 `verify=False`，且没有任何开关能打开校验 |
| 关键词搜索 | ❌ | ✅ | ✅ | fork 走 1ms API；GUI 带分页（20/页） |
| tag 列表 | ❌ | ✅ | ✅ | CLI 只认手输 tag |
| 进度显示 | ✅ | ✅ | ✅ | CLI/fork 是 rich 多行进度条 + 平均速度 + 总耗时；GUI 是自绘 HTML 面板，`time.sleep(0.5)` 轮询刷新。**三者都没有 ETA，也没有整体百分比** |
| 本地 tar 管理 | ❌ | ❌ | ✅ | 只有 GUI 能列出 `.tar`、点浏览器下载、下拉删除；删除时 `os.path.join(out_dir, filename)` 无路径穿越校验 |
| tar 打包（`docker load` 兼容） | ✅ | ✅ | ✅ | 各自实现 fakeid/parent 链、`manifest.json`、`repositories`；这是唯一三项都 ✅ 的能力 |
| CLI 参数体系 | ✅ | ✅ | ❌ | CLI 约 15 个参数；fork 约 11 个（README 宣传的 `-i/--image`、`-w/--workers` **在代码里不存在**）；GUI 无命令行参数 |
| 图形界面 | ❌ | ❌ | ✅ | 仅 GUI：Gradio，`0.0.0.0:7860`，`inbrowser=True` |
| CI 模式 | ⚠️ | ❌ | ❌ | CLI 有 `--ci`（跳过交互与进度动画）并维护退出码；fork 无 `--ci`，**且完全没有退出码管理**——错误路径 `return` 之后进程照样 exit 0 |

结论：**16 项能力里，只有"tar 打包"三项都完整**。其余每一项都至少有一份实现是缺的或坏的——
这正是"每个 bug 要修 1~3 次、而实际上一次都没修全"的量化版本。

### 2.2 自动化测试的实际覆盖

`../legacy/tests/` 共 56 个用例（`test_core.py` 31 + `test_unit_download.py` 18 + `test_e2e.py` 7），
全部 `import docker_image_puller`，E2E 也是以子进程跑 `docker_image_puller.py`。
**1ms fork 与 Gradio GUI 零覆盖**——它们的缺陷（§3）不是"漏测"，是"从来没被测过"。

---

## 3. 原有实现的问题清单

### 3.1 重复与漂移

| 问题 | 影响 | 新方案如何解决 |
|---|---|---|
| 一个引擎三份拷贝（1511 / 1369 / 707 行，约 700 行近乎逐行重复） | 任何一处修复都要在 1~3 个文件里重做；实际结果是只在一个文件里修 | 引擎收敛为 `internal/registry` + `internal/puller`，生命周期归 `internal/tasks`；CLI 与 GUI 都是薄客户端，走同一条代码路径 |
| README 漂移：1ms 段落宣传 `-i/--image`、`-w/--workers`，代码里没有 | 用户照文档执行 → `unrecognized arguments` | 参数面只由 `cmd/server` 的子命令表定义，与 CONTRACT §8 一一对应；文档只描述一个实现 |
| 版本号自相矛盾：`legacy/version.txt` 里 `filevers=(1,6,1,0)` / `FileVersion 1.9.0.0` / `ProductVersion 1.8.0.0`，脚本内又是 `VERSION="v1.9.0"` 与 `"v1.0.0-1ms"` | 无法判断某个 exe 到底是哪版，问题定位靠猜 | 单一 `internal/buildinfo`（Version/Commit/Date）由 ldflags 注入；`dockerpull version`、`dockerpull serve -version` 与 `GET /api/status` 输出同一份 |
| 三份实现的行为差异没有任何文档 | 用户以为"GUI 和 CLI 一样"，其实校验、认证、退出码都不同 | 本文档 §2/§3 把差异写成矩阵；新实现只有一个行为 |

### 3.2 正确性

| 问题 | 影响 | 新方案如何解决 |
|---|---|---|
| GUI 从不校验 SHA256（`expected_digest` 收了没用） | 损坏/被篡改的 tar 被当成成功产物，直到 `docker load` 才炸，且现场已经换到内网 | 校验是 `internal/puller` 的硬路径：每个 blob 下完即比对 digest，失配则删文件 + 清 chunk ledger + 重下（计入 `MaxRetries`） |
| 1ms fork 与 GUI 缺"服务器忽略 Range 返回 200"判别，却仍以追加模式写 | **静默损坏**：半截文件 + 全文，长度对不上，只有校验（GUI 还没有校验）能发现 | `Pull` 明确三分支：206 → 从偏移追加；200 → 截断重下；416 → 视为已完成并校验（CONTRACT §3） |
| fork 与 GUI 用 `split('"')[1]`/`[3]` 解析 `WWW-Authenticate` | 遇到 `Basic realm="x"` 形式的 challenge 直接 IndexError 崩溃 | `registry.ParseWWWAuthenticate` 返回结构化 `AuthChallenge{Scheme,Realm,Service,Scope}`，Basic/unknown 走 Basic 兜底 |
| fork 无退出码管理，失败也 exit 0 | CI 里失败变绿灯，坏产物被发布 | CLI 定义 `0` 成功 / `1` 失败 / `2` 用法错误，且"**绝不静默 0**" |
| 分片与流式两条写盘路径在三份实现里语义漂移（分片模式不读已存在的半截文件） | 续传行为取决于 blob 大小，用户不可预期 | 两条路径共用同一份 `State`：流式按 partial 文件大小续，分片按持久化 chunk ledger 续 |
| 主线 CLI 无条件取两遍 manifest（401 重认证分支之后又取一次，即使首次已经成功） | 多一次往返、弱网下多一次失败面 | `ResolveImage` 一次解析 index → 平台子 manifest，链式取一遍 |

### 3.3 可用性

| 问题 | 影响 | 新方案如何解决 |
|---|---|---|
| 进度无 ETA、无整体百分比（旧 README 却宣称有"总进度"） | 拉几百 MB 的镜像时不知道还要多久 | `internal/tasks` 汇总 `totalBytes/downloadedBytes` 与速度 → 整体百分比 + ETA，经 SSE `task.progress` 推给 GUI，CLI 走同一份数据 |
| `session.timeout = (60, 600)` 是空操作（requests 的 `Session.timeout` 属性不生效） | 配置看着生效其实没有，超时行为只能靠每次调用的字面量 | 超时是显式配置：`ClientOptions.Timeout`（默认 60s）/ puller 的每次请求参数 |
| 多源 = 从菜单里挑一个字符串 + 硬编码 host 替换 | 没有故障转移；某个源挂了只能手改参数重来 | 契约只承诺"镜像源选择 + 每个任务可选源"（`--mirror HOST|ID`、`sources` 表、`/settings`），**不承诺自动 failover / 竞速**——见 §10 |
| "断点续传"提示只在 CLI 有、GUI 无 | GUI 用户不知道程序在续传还是重下 | 续传状态落在 `progress.json` 并由 `tasks` 暴露，GUI 的 `/tasks` 与 CLI 看到同一份层状态 |
| `--list-arch` 只在 CLI 存在 | GUI 用户要先"假装下载"才能看到架构 | `POST /api/images/inspect` + `ScanPlatforms` 是同一个原语，GUI 选架构对话框直接调用 |

### 3.4 安全性

| 问题 | 影响 | 新方案如何解决 |
|---|---|---|
| 三份实现所有请求 `verify=False`，CLI/fork 没有开关 | 中间人可替换镜像内容，而"内网传镜像"恰恰依赖内容可信 | `verify_tls` 默认 **true**（CONTRACT §4 设置项默认值），CLI `--no-verify-tls`、GUI `/settings` 可显式关闭，`--insecure` 只表示"registry 走明文 HTTP" |
| GUI `delete_local_tar(name)` 直接把用户给的文件名 `os.path.join(out_dir, name)` 后 `os.remove` | 下拉框取值可被构造为 `../../x`，删除任意文件 | 产物删除按 `artifacts` 表记录的 id 删除，路径取自库内记录并做前缀校验；接口不再接受任意文件名 |
| 代理密码在 GUI 里是普通输入框，凭据只活在进程内存 | 单机工具可接受，但没有任何"凭据加密存储" | **新版本仍未加密**：凭据面收敛为 `-u/-p` 与代理 URL，加密存储列入 §10 待办，不假装已经解决 |
| `-p` 明文出现在命令行/CI 日志里 | 凭据泄漏到 shell history 与 CI 输出 | 兼容保留（老用户脚本不能失效），但 GUI 路径与 `settings` 不需要在命令行暴露凭据 |

### 3.5 可维护性

| 问题 | 影响 | 新方案如何解决 |
|---|---|---|
| `is_resume` / `retry_count` 字段三份都定义、从未赋值 | 对应的格式化分支（`🔄{retry_count}`、`is_resume` 标记）永远不会触发，读代码被误导 | 状态用 `LayerState.Status` 枚举表达，进度由 `Sink` 回调 + `tasks` 汇总产生，没有"死字段" |
| 未使用的 `get_file_size` 三份都留着 | 死代码要靠人读出来 | 引擎只在一个包里，未使用即删（编译器 + `go vet` 兜底） |
| 认证 / 下载 / 打包 / UI 混在一个 1500 行的文件里 | 改一处要通读全文件，评审无法聚焦 | 五个包 + 显式接口：`Searcher`、`Sink`、`Store` |
| 56 个用例只打主线 CLI | 修 bug 只测到 1/3 的实现 | 测试打共享引擎（`internal/registry`、`internal/puller`、`internal/tasks`），CLI 与 GUI 天然同源 |

---

## 4. 新功能架构

### 4.1 模块图

```
             ┌───────────────────────────┐        ┌────────────────────────────┐
             │  CLI      cmd/server      │        │  GUI      web/ (Next.js)   │
  终端 ──────▶  pull / search / tags     │        │  /  /search  /tasks        │
             │  tasks / serve / version  │        │  /artifacts  /settings     │
             └─────────────┬─────────────┘        └──────────────┬─────────────┘
                           │ 直接调用（同进程）                   │ HTTP /api/* + SSE
                           │                                      │
                           ▼                                      ▼
                    ┌──────────────────────────────────────────────────────┐
                    │  internal/server                                     │
                    │  声明式路由表 + 每个域一个 handler 文件                │
                    │  统一封套 writeOK/writeError/readJSON                 │
                    │  GET /api/events → SSE                               │
                    └───────┬──────────────────────────┬───────────────────┘
                            │                          │
                ┌───────────▼───────────┐   ┌──────────▼──────────────────┐
                │  internal/tasks       │   │  internal/store             │
                │  任务生命周期          │   │  tasks / task_layers        │
                │  pause/resume/retry   │   │  sources / settings         │
                │  进度扇出（Sink→SSE）  │   │  artifacts                  │
                └───────────┬───────────┘   └─────────────────────────────┘
                            │
                ┌───────────▼───────────┐
                │  internal/puller      │
                │  可续传下载 + 校验     │
                │  docker load 兼容打包  │
                └───────────┬───────────┘
                ┌───────────▼───────────┐
                │  internal/registry    │
                │  引用解析 / 认证        │
                │  清单 / 架构 / 搜索     │
                └───────────────────────┘
```

一句话：**一个引擎（registry + puller + tasks + store），两个前端（CLI、GUI）**，
两个前端共享同一个 `internal/server` 路由表，因此 API 行为、错误信息、进度语义永远一致。
没有认证、没有多租户，单用户本机工具（CONTRACT 开头即写明）。

### 4.2 F1 镜像搜索

**职责**：用关键词在多个搜索源里找镜像、列 tag，为下载页提供候选。

**关键能力**

- 多搜索源：内置 `dockerhub`、`1ms`（`BuiltinSearchSources`），落库到 `sources` 表，可在 `/settings` 增删改/启停/排序。
- `Searcher` 接口统一分页：`Search(ctx, keyword, page, pageSize)` / `Tags(ctx, repository)`；`PickSearcher` 处理未知或空 id（回落默认源）。
- 结果字段：名称、仓库、镜像名、描述、Star、拉取量、更新日期、是否官方。
- tag 列表返回平台信息（`TagInfo.Platforms`），供选架构对话框预填。
- CLI 支持 `--json`，方便脚本消费。

**依赖的包**：`internal/registry`（`Searcher`、`SearchSources`、`PickSearcher`、`SearchPage`、`TagInfo`）；
搜索源配置来自 `internal/store`（`sources` 表）。

**对应 CLI 命令**

```bash
dockerpull search -k nginx --source dockerhub --page 1 --page-size 20
dockerpull search -k nginx --source 1ms --json
dockerpull tags -i nginx --source dockerhub
```

**对应 GUI 页面**：`/search`（`web/src/app/search/page.tsx`）。

### 4.3 F2 镜像下载

**职责**：解析引用 → 解析架构 → 认证 → 拉 layer/config → 校验 → 组装 `docker load` 兼容 tar。

**关键能力**

- 引用解析 `Parse(s, customRegistry)`：补默认 registry（`registry-1.docker.io`）、默认 `library/`、默认 tag `latest`；
  `Reference()` 在有 digest 时优先用 digest；`RepoTag()` 在 Docker Hub 上剥掉 `library/`。
- **真 digest 支持**：`nginx@sha256:...` 解析为 `{Repository: nginx, Digest: sha256:...}`（旧版把它解析成 image=`nginx@sha256`）。
- 架构解析 `ResolveArch`：精确 `os/arch[/variant]` → 精确 arch → 规范别名组（`ArchAliases`）→ 双向子串 → arm variant。
- registry 认证：`AuthHeader` 按 challenge 取 Bearer token，Basic/unknown 走 Basic；`-u/-p` 提供凭据。
- 镜像源：`BuiltinMirrors` 10 个（Docker Hub 官方 / 1ms / 南大 / 轩辕 ×2 / DaoCloud ×5）+ 用户自定义；
  `--mirror HOST|ID` 是**每个任务**的选择，服务端另有 `default_mirror`。
- 传输安全：`verify_tls` 默认开（`--no-verify-tls` 关）；`--insecure` 表示该 registry 走明文 HTTP。
- 代理三态：`system`（跟随环境变量）/ `none`（直连）/ `custom`（`proxy_url`）。
- 多架构：`ResolveImage` 拉 index → 取平台子 manifest；`ScanPlatforms` 供 GUI/CLI 的 `--list-arch`。
- 大 blob 分片 + 并发：`Workers`（默认 4）、`ChunkSize`（默认 10 MiB）、`ChunkThreshold`（默认 50 MiB）。
- 产物命名固定可预期：`TarName` = `<safe_repo>_<tag>_<safe_arch>.tar`；`DefaultOutputDir` = `<base>/<safe_repo>_<tag>_<safe_arch>`。

**依赖的包**：`internal/registry`（`Client`、`Manifest`、`Descriptor`、`OpenBlob`、`HeadBlob`）、
`internal/puller`（`Options`、`Pull`、`TarName`、`DefaultOutputDir`、`Result`）、
`internal/tasks`（任务落库与事件）、`internal/store`（任务行）。

**对应 CLI 命令**

```bash
dockerpull pull -i nginx:1.26 -a amd64 -o ./downloads --workers 4
dockerpull pull -i harbor.example.com/library/nginx:1.26 -u admin -p 'secret' -r harbor.example.com
dockerpull pull -i alpine:latest --mirror 1ms -o ./out
dockerpull pull -i nginx:latest --list-arch
```

**对应 GUI 页面**：`/search`（搜索结果 → tag/架构选择对话框 → 开始下载，`POST /api/tasks`）。

### 4.4 F3 断点续传

**职责**：进程被杀 / 网络断 / 手动暂停之后，从上次的字节位置继续，不重下已经完成的 blob。

**关键能力**（详细语义见 §5）

- 任务状态持久化在 `<WorkDir>/progress.json`（原子写：tmp + rename）。
- 流式路径按半截文件大小续传；分片路径按持久化的 chunk ledger 续传。
- 每个完成的 blob 都做 digest 校验，失败即删文件 + 清 ledger + 重下。
- **跨进程重启有效**；暂停保留状态，重试清空状态从头开始。

**依赖的包**：`internal/puller`（`LoadState`、`SaveState`、`State`、`LayerState`、`ChunkState`）、
`internal/tasks`（`Pause`、`Resume`、`Retry`、`Shutdown`）。

**对应 CLI 命令**

```bash
# 同一 -o 目录再跑一次即续传（WorkDir 不变）
dockerpull pull -i nginx:1.26 -a amd64 -o ./downloads
# 管理某个任务的续传
dockerpull tasks list
dockerpull tasks resume <id>
```

**对应 GUI 页面**：`/tasks`（`继续` / `暂停` / `重试` 按钮）。

### 4.5 F4 进度管理

**职责**：任务生命周期 + 实时进度扇出到 CLI、GUI、SSE 订阅者。

**关键能力**

- 状态机 `pending → running → paused | succeeded | failed | canceled`（`internal/tasks` 常量）。
- 两种回调：`OnState`（低频：状态迁移、尺寸发现、完成）与 `OnProgress`（高频：只带字节数）。
- 任务与层落库（`tasks` / `task_layers`），刷新页面/重启进程都能还原。
- 事件：`task.created` / `task.updated` / `task.progress` / `task.finished` / `artifact.created` /
  `artifact.deleted` / `source.changed` / `settings.changed`；`task.updated` 合并到约 ≤5 次/秒/任务。
- 进度含：每层进度、整体百分比、速度、总量、ETA。
- CLI 保留旧中文交互串与进度条；`--ci` / `--quiet` 关闭动画，输出适合日志。
- 唯一的 `events.Hub` 由 `internal/app.Boot` 创建；桌面壳（Wails）与浏览器共用同一路 SSE。

**依赖的包**：`internal/tasks`（`Manager`、`Task`、`StartSpec`）、`internal/events`（`Hub`）、
`internal/server`（`/api/events`、`/api/tasks*`）、`internal/store`。

**对应 CLI 命令**

```bash
dockerpull tasks list
dockerpull tasks show <id>
dockerpull tasks pause <id> | resume <id> | cancel <id> | retry <id>
dockerpull tasks rm <id> --files
```

**对应 GUI 页面**：`/tasks`（任务表 + 每层进度条 + 操作按钮）；`/`（概览页的实时流与统计卡）。

### 4.6 F5 本地镜像包管理

**职责**：管理 `output_dir` 下的 `.tar` 产物——列出、下载、删除、与磁盘对账。

**关键能力**

- `artifacts` 表：文件名、绝对路径、repository、tag、platform、大小、来源 task、创建时间。
- `ScanArtifacts` 把表与 `output_dir` 目录对账（补上手工放进来的 tar、清掉已不存在的行）。
- 浏览器直接下载：`GET /api/artifacts/{id}` 返回 `application/x-tar`。
- 删除按 id，路径来自库内记录，不再接受用户提供的文件名（修掉旧 GUI 的路径穿越）。
- 页面给出 `docker load -i <file>` 的导入提示。
- 统计：`GET /api/stats` 给出产物数量与总字节数，供概览页使用。

**依赖的包**：`internal/store`（`Artifact`、`ArtifactStore`）、`internal/tasks`（`ScanArtifacts`）、
`internal/server`。

**对应 CLI 命令**：`dockerpull tasks rm <id> --files`（连带删除产物）。
**CLI 目前没有独立的 `artifacts` 子命令**——列表/下载/删除只在 GUI 提供，补 CLI 子命令列入 §10。

**对应 GUI 页面**：`/artifacts`（`web/src/app/artifacts/page.tsx`）。

### 4.7 附带的设置与概览

- `/settings`：网络（代理模式/URL、TLS 校验、镜像源）、下载（输出目录、并发、分片阈值、重试）、
  数据源管理、外观。读写 `GET/PUT /api/settings`、`/api/sources`。
- `/`：概览——任务/产物统计卡、最近任务、实时事件流。
- 这两个页面不属于五功能域，但它们是"配置只有一处"的落点：CLI 与服务端读同一批设置。

---

## 5. 断点续传的准确语义

### 5.1 状态放在哪里

| 内容 | 位置 |
|---|---|
| 任务状态（含续传账本） | `<WorkDir>/progress.json` |
| 未完成 / 已完成的 blob | `<WorkDir>/blobs/<sha256_hex>`（半截文件就是续传起点） |
| blob 已校验的封条 | `<WorkDir>/blobs/<sha256_hex>.ok`（只有摘要校验通过后才写） |
| 分片模式的已完成分片 | `<WorkDir>/chunks/<sha256_hex>/chunk_%05d` |
| 打包暂存区 | `<WorkDir>/stage/`（tar 的根，成功后删除） |
| 任务元数据（WorkDir、TarPath、字节数、状态） | 数据库 `tasks` / `task_layers` 表 |
| 最终产物 | `<OutputDir>/<TarName>` |

`WorkDir` 与 `OutputDir` 是两个目录：前者是"施工地"（半成品 + 账本），后者只放成品 tar。
续传的定义就是**把新的 job 指到同一个 WorkDir**。

三个子目录（`blobs/`、`chunks/`、`stage/`）是有意分开的：tar 只从 `stage/` 打包
（等价于 `tar -C <WorkDir>/stage -cf out.tar .`），所以 `progress.json`、blob 缓存和分片临时文件
都不可能漏进镜像包里。`stage/` 打包成功即删除，`blobs/` 保留 —— 重新导出或重新拉取同一镜像不需要网络。

### 5.2 持久化了什么

`progress.json` 是一份 `State`：

- 任务级：`Ref`、`Platform`、`Status`、`Error`、`TotalBytes`、`DownloadedBytes`、`StartedAt`、`UpdatedAt`。
- 层/配置级（`Layers[]`）：`Digest`、`Kind`（`layer` / `config`）、`Name`、`Size`、`Downloaded`、`Status`、`Error`。
- 分片账本（`Chunks[]`，只有分片模式有）：`Index`、`Start`、`End`（闭区间）、`Done`。

写入是**原子**的（写 tmp 再 rename），所以进程被 kill 不会留下半个 JSON。

### 5.3 三种 HTTP 响应各代表什么

请求 `Range: bytes=<已下载字节数>-` 之后：

| 响应 | 含义 | 处理 |
|---|---|---|
| `206 Partial Content` | 服务器接受了续传 | 从偏移处追加写；总大小取 `Content-Range` |
| `200 OK` | 服务器**忽略**了 Range，返回的是全文 | **截断文件，从 0 重新写**（绝不追加——旧 fork/GUI 就是在这里静默损坏） |
| `416 Range Not Satisfiable` | 请求的起点已经越过文件末尾 | 认为该 blob 已经下载完整，直接进入校验/完成 |

分片模式下每个分片是一次独立的 Range GET，各写各的 `chunk_%05d`，任何一片失败只重下那一片。

### 5.4 进程重启后还剩什么

- 账本：`progress.json` 完整保留（原子写）。
- 半截数据：`blobs/<sha256_hex>`（流式路径的半截文件）与 `chunks/<sha256_hex>/chunk_*` 都留着。
- 已完成并**校验通过**的 blob 旁边有一个封条 `<sha256_hex>.ok`，它是"这些字节可信"的唯一凭据。
- 因此重启后对同一个 WorkDir 再跑一次 `Pull`：
  - 有 `.ok` 且 blob 文件非空 → 该层报为 `skipped`，**不再重算哈希**（重算一个 500MB 的层要几秒到几十秒，
    而封条只在摘要校验通过后才写）。这一条也是 `Result.Completed`（"复用了 N 层"）的来源。
  - 没有 `.ok` → 正常下载；已有字节按偏移续传，不会从 0 重来。
  - 有 `.ok` 但 blob 文件不在了（手工删过/清理过）→ 丢掉陈旧封条，重新下载。
- 匹配规则：`LoadState(workDir, ref, platform)` 在 **Ref/Platform 与本次请求不一致时返回 `(nil, nil)`**，
  即"陈旧状态一律不许复用"；文件不存在、读不出来或 JSON 损坏也当作全新任务，不报错。
- 已知缺口：`LoadState` 只比对 `Ref` 字符串（`registry/repository:tag`）与架构，**不含 tag 指向的 digest**。
  同一个 tag 被重新 push 后，旧账本仍会被当作有效。用 `@sha256:` 固定摘要的请求不受影响。
  见 §11 后续可做。

### 5.5 "继续"与"重试"的区别

| 操作 | 对 progress.json | 对半截文件 | 从哪继续 |
|---|---|---|---|
| 继续 / `resume`（`tasks resume <id>`） | 沿用 | 沿用 | 最后一个已完成字节 |
| 暂停 / `pause`（`tasks pause <id>`） | 保留（仅停掉运行中的 job） | 保留 | 之后 resume 从原偏移继续 |
| 取消 / `cancel` | 保留（状态置 `canceled`） | 保留 | 之后 resume 仍可续 |
| 重试 / `retry`（`tasks retry <id>`） | **清空** | 丢弃/重置 | **字节 0**，整任务重来 |
| 删除 / `rm <id> [--files]` | 删除 | `--files` 时连同 WorkDir 与产物一起删 | 无 |

`Manager.Shutdown` 会停掉所有运行中的任务，**留下可续传的状态**（不是清空）。

### 5.6 明确**不**保证的事情

- **不做 digest 级别的失效判定**：`LoadState` 只比 `Ref` + `Platform` 两个字符串。
  如果同一个 tag 被上游重新推送（digest 变了），tag 引用的 `Ref` 不变，旧账本仍会被视为有效。
  （用 `repo@sha256:...` 引用时 `Ref` 字符串本身含 digest，因此能被正确判定为新任务——
  这个空档只在 tag 引用上出现。改进方向见 §10。）
- **不跨 WorkDir / 不跨机器**：换了输出目录、换了机器就是新任务，进度不迁移。
- **不保证"永不重下"**：服务器忽略 Range（200）时会截断重下；校验失败会重下；
  小于 `ChunkThreshold` 的 blob 一次性下载，中断即整块重来。
- **不做跨镜像的层去重缓存**：同一个 layer 被多个镜像共用时，会各自下载到各自的 WorkDir。
- **不做限速与并发自适应**：`workers` 是你给多少就是多少。
- **换源不等于换路续传**：`--mirror` 变了就是一个新任务（registry host 不同，blob URL 与认证都不同）。

---

## 6. API 一览

统一封套：`{"ok":true,...}` / `{"ok":false,"error":"..."}`；**无认证**，本机浏览器/CLI 直接调用即可。

### 6.1 健康与状态

| 路由 | 用途 |
|---|---|
| `GET /healthz` `/livez` `/readyz` | 存活/就绪探针（容器与运维用） |
| `GET /api/status` | 版本、dataDir、outputDir、运行平台、`auth:false`；前端启动自检 |
| `GET /api/stats` | 概览页统计：任务数（总/运行/成功/失败）、累计字节、产物数与总大小 |

### 6.2 搜索与检查

| 路由 | 用途 |
|---|---|
| `GET /api/search?q=&source=&page=&pageSize=` | 关键词搜索（按源分页） |
| `GET /api/tags?repository=&source=` | 某仓库的 tag 列表（含平台信息） |
| `POST /api/images/inspect` | 给定镜像串解析引用、取清单、列出可用架构（不下载） |

### 6.3 任务（下载 / 续传 / 进度）

| 路由 | 用途 |
|---|---|
| `GET /api/tasks` | 任务列表 |
| `POST /api/tasks` | 新建下载任务（`image` / `registry` / `platform` / `workers` / `insecure` / `useHTTP` / `verifyTls` / `outputDir`），立即返回 |
| `GET /api/tasks/{id}` | 单任务详情（含每层状态） |
| `POST /api/tasks/{id}/pause` | 暂停，保留 WorkDir + 账本 |
| `POST /api/tasks/{id}/resume` | 从同一 WorkDir 续传 |
| `POST /api/tasks/{id}/cancel` | 取消（状态 `canceled`，仍可续） |
| `POST /api/tasks/{id}/retry` | 清空账本，从字节 0 重来 |
| `DELETE /api/tasks/{id}?files=true` | 删除任务；`files=true` 时连 WorkDir 与产物一起删 |

### 6.4 产物

| 路由 | 用途 |
|---|---|
| `GET /api/artifacts` | 本地 tar 列表 |
| `GET /api/artifacts/{id}` | 直接下载该 tar（`application/x-tar`） |
| `DELETE /api/artifacts/{id}` | 删除该 tar（按 id，路径取库内记录） |

### 6.5 数据源与设置

| 路由 | 用途 |
|---|---|
| `GET /api/sources?kind=` | 列出搜索源（`search`）或镜像源（`mirror`） |
| `POST /api/sources` | 新增源 |
| `PUT /api/sources/{id}` | 修改源（名称/URL/host/启用/优先级/默认） |
| `DELETE /api/sources/{id}` | 删除源 |
| `GET /api/settings` | 读取全部设置（key→value） |
| `PUT /api/settings` | 批量 upsert 设置 |

### 6.6 实时

| 路由 | 用途 |
|---|---|
| `GET /api/events` | SSE 事件流：`task.created/updated/progress/finished`、`artifact.created/deleted`、`source.changed`、`settings.changed` |

---

## 7. CLI 一览

二进制 `dockerpull`（`cmd/server`）。**没有子命令时等价于 `serve`**，所以裸跑就是启动 Web 服务。
退出码：`0` 成功、`1` 失败、`2` 用法错误——**绝不静默返回 0**（这是对旧 fork 的直接修正）。

### 7.1 命令面

```
dockerpull [serve] [-port N] [-data-dir PATH] [-dev-proxy URL] [-version]
dockerpull pull   -i <image> [-a amd64] [-o DIR] [--workers N] [--insecure]
                  [-r REGISTRY] [-u USER] [-p PASS] [--mirror HOST|ID]
                  [--proxy URL] [--no-proxy] [--no-verify-tls]
                  [--ci] [--quiet] [--list-arch] [--debug]
dockerpull search -k <keyword> [--source ID] [--page N] [--page-size N] [--json]
                  [--no-download] [--tag T] [-a ARCH] [-o DIR]
dockerpull tags   -i <image> [--source ID] [--json]
dockerpull tasks  [list | show <id> | pause <id> | resume <id> | cancel <id> | retry <id>
                  | rm <id> [--files]]
dockerpull version
```

### 7.2 常用示例

```bash
# 启动 GUI（默认子命令）：浏览器打开 http://localhost:8080
./dockerpull
./dockerpull serve -port 9090 -data-dir /srv/dockerpull

# 拉取并导出（默认 amd64、默认输出目录、4 并发）
./dockerpull pull -i nginx:1.26 -o ./downloads

# 指定架构 + 走 1ms 加速源
./dockerpull pull -i alpine:latest -a arm64 --mirror 1ms -o ./out

# 私有仓库（Bearer 或 Basic 自动识别）
./dockerpull pull -i harbor.example.com/library/nginx:1.26 -u admin -p 'secret'

# 只列架构，不下载（退出码 0）
./dockerpull pull -i nginx:latest --list-arch

# CI：无交互、无动画、失败即非 0
./dockerpull pull -i nginx:1.26 --ci --quiet -o ./dist

# 搜索与 tag（可 --json 给脚本用）
./dockerpull search -k redis --source dockerhub --json
./dockerpull tags -i redis --source 1ms

# 任务管理（含续传）
./dockerpull tasks list
./dockerpull tasks pause 3f2a1b
./dockerpull tasks resume 3f2a1b     # 从上次字节继续
./dockerpull tasks retry  3f2a1b     # 清空账本，从 0 重来

# 版本
./dockerpull version
```

### 7.3 旧版兼容参数

老用户脚本可以原地替换 `DockerPull.exe` → `dockerpull pull`，这些参数语义保持一致：

`-i` `-a` `-o` `-r` `-u` `-p` `--workers` `--ci` `--quiet` `--list-arch` `--insecure` `--proxy` `--no-proxy` `--debug`

新增（旧版没有）：`--mirror HOST|ID`（显式选源）、`--no-verify-tls`（关掉证书校验——旧版是**默认关且无法打开**）。

### 7.4 交互模式的输出

不带齐参数时，`pull` 进入交互模式，**保留旧版的中文串**（这些是"字面输出"，emoji 是程序真的会打印的）：

```
🚀 Docker 镜像拉取工具
请输入 Docker 镜像名称（例如：nginx:latest 或 harbor.abc.com/abc/nginx:1.26.0）：
📋 当前可用架构：amd64, arm64, armv7, ppc64le, s390x
📎 nginx:latest 检测到已下载 12.3 MB，尝试断点续传...
✅ 镜像 nginx:latest 下载完成！
💡 导入命令: docker load -i nginx_latest_amd64.tar
⏱️  总耗时: 00:01:23
```

---

## 8. 配置项

启动配置是 **env-only**（`APP_*`），应用配置存在 `settings` 表里、由 GUI `/settings` 与 CLI 共同读写。

### 8.1 应用设置（`settings` 表，CONTRACT §4）

| Key | 默认值 | 含义 |
|---|---|---|
| `output_dir` | `<dataDir>/downloads` | `.tar` 产物落地目录 |
| `workers` | `4` | 并发下载的层数 |
| `verify_tls` | `true` | 是否校验 TLS 证书 |
| `proxy_mode` | `system` | `system` \| `none` \| `custom` |
| `proxy_url` | `""` | `proxy_mode=custom` 时使用 |
| `default_mirror` | `dockerhub` | 引用里没写 registry 时用哪个镜像源 |
| `default_search_source` | `dockerhub` | 默认搜索后端 |
| `chunk_threshold_mb` | `50` | 超过多少 MB 用分片模式 |
| `max_retries` | `10` | 单个 blob 的重试预算 |

### 8.2 进程引导配置（`APP_*` 环境变量）

| 变量 | 默认值 | 含义 |
|---|---|---|
| `APP_PORT` | `8080` | HTTP 端口 |
| `APP_BIND` | `loopback` | `loopback`（只监听本机）或 `all` |
| `APP_DATA_DIR` | `./data` | 数据目录（SQLite 与默认下载目录的基准） |
| `APP_DB_TYPE` | `sqlite` | `sqlite` 或 `postgres` |
| `APP_DB_DSN` | 空 | Postgres DSN，或显式 SQLite 路径 |
| `APP_DB_AUTO_MIGRATE` | `true` | 启动时自动迁移 |
| `APP_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `APP_DEV_PROXY` | 空 | 指向 `next dev`，把 UI 反向代理进来（开发用） |

> 旧模板的 `APP_COOKIE_SECURE` / `APP_RATE_LIMIT_RPM` 是认证/限流开关，认证移除后不再有意义。

---

## 9. 迁移对照表

### 9.1 旧 CLI 参数 → 新 CLI

| 旧（`docker_image_puller.py`） | 新（`dockerpull ...`） | 备注 |
|---|---|---|
| `-i, --image` | `pull -i <image>` | 语义不变；新增 `repo@sha256:...` 支持 |
| `-a, --arch` | `pull -a <arch>` | 别名更全（`x86_64`/`aarch64`/`armhf`…） |
| `-o, --output` | `pull -o <dir>` | 语义不变 |
| `-r, --custom-registry` | `pull -r <registry>` | 语义不变 |
| `-u, --username` / `-p, --password` | `pull -u` / `-p` | Bearer 与 Basic 都会用；GUI 路径无需在命令行暴露凭据 |
| `-q, --quiet` | `pull --quiet` | 语义不变 |
| `--debug` | `pull --debug` | 也可用 `APP_LOG_LEVEL=debug` |
| `--workers N` | `pull --workers N` | 语义不变 |
| `--ci` | `pull --ci` | 跳过交互与动画；**并且现在真的会给出非 0 退出码** |
| `--list-arch` | `pull --list-arch` | 语义不变（只列架构后退出） |
| `--proxy URL` | `pull --proxy URL` | 语义不变 |
| `--no-proxy` | `pull --no-proxy` | 语义不变 |
| `--insecure`（registry 走 HTTP） | `pull --insecure` | 语义不变 |
| `-v, --version` | `dockerpull version` | 版本来自 `internal/buildinfo` |
| —— | `pull --mirror HOST\|ID` | **新增**：显式选镜像源，替代"菜单里挑一个字符串" |
| —— | `pull --no-verify-tls` | **新增**：旧版默认不校验且打不开校验，现在默认校验、可显式关 |
| 1ms fork `-k, --keyword` | `search -k <keyword>` | 语义不变 |
| 1ms fork `-t, --tag` | `search --tag T` 或 `pull -i repo:T` | tag 现在是引用的一部分 |
| 1ms fork `--page-size` | `search --page-size N` | 语义不变 |
| 1ms fork `--page` | `search --page N` | 语义不变 |
| 1ms fork `--registry <host>` | `pull --mirror <host>` / `search --source` | 源的分工改成"镜像源"与"搜索源"两类 |
| 1ms fork `--api <url>` | `/settings` 里的数据源记录 | URL 落库、可在界面改，不再是命令行参数 |
| 1ms fork `--no-download` | `search --no-download` | 保留：`--no-download` 只搜不下。不加该参数时，搜索结果若只有一条会直接下载，多条则提示输入序号（非交互场景请显式用 `--no-download` 或 `--json`） |
| 1ms fork `--select-index` | **丢弃** | 见 §9.3 |
| 1ms fork `--page`（配合选结果） | `search --page N` | 语义改为"搜索结果的页码" |
| GUI `python app.py`（:7860） | `dockerpull`（:8080） | 单一入口，CLI 与 GUI 同源 |

### 9.2 旧 GUI 控件 → 新 GUI

| 旧 Gradio 控件（`app.py`） | 新页面/接口 | 备注 |
|---|---|---|
| `📦 镜像保存目录` 文本框 | `/settings` → `output_dir` | 落库，不再每次手填 |
| 源下拉框（硬编码列表） | `/settings` → 数据源管理 + `default_mirror` | 源可增删改、可启停、可排序 |
| `🔐 开启 SSL 证书验证` 勾选框 | `/settings` → `verify_tls`（默认开） | 唯一在旧版就生效的安全开关，保留并成为全局默认 |
| `📡 代理模式` 单选 + 代理地址/用户名/密码 | `/settings` → `proxy_mode` + `proxy_url` | 代理账号密码**不落库**：需要认证的代理请把凭据写进 URL 或走系统代理 |
| 关键词框 + `🔍 搜索` | `/search` | 多搜索源可选 |
| 结果表格 + 上一页/下一页 | `/search` 结果表 + 分页 | 走 `GET /api/search` |
| 当前选中目标文本框 | `/search` 表格选中行 | 少一次"复制粘贴" |
| Tag 下拉框 | `/search` 内 tag 选择 | 走 `GET /api/tags` |
| Arch 下拉框（**写死的候选**） | `/search` 内架构选择 | 候选来自镜像 manifest（`inspect` / `ScanPlatforms`），不再写死 |
| `⬇️ 开始下载 & 导出` | `/search` → 开始下载（`POST /api/tasks`） | 立即返回任务 id，不等下载完 |
| `运行日志 & 进度` HTML 面板（0.5s 轮询） | `/tasks` + SSE `/api/events` | 换成服务端推送，见 §9.3 |
| 本地 tar 列表（`gr.File`） | `/artifacts` | 列表来自 `artifacts` 表 + 目录对账 |
| `🔄 刷新本地列表` | `/artifacts` 自动更新 | `artifact.created/deleted` 事件驱动 |
| 删除下拉框 + `❌ 删除选中文件` | `/artifacts` 删除按钮 | **按 id 删除并做路径前缀校验**，修掉路径穿越 |

### 9.3 有意丢弃的行为

| 丢弃的东西 | 为什么 | 替代 |
|---|---|---|
| 1ms 的 `--select-index`（按"当前页第 N 个结果"选镜像） | 位置索引脆弱：搜索源换页/换排序后指向的镜像就变了，脚本不可复现 | 结构化两步：`search -k k --json` 拿结果 → `pull -i repo:tag`（`--page` 保留为搜索页码） |
| Gradio 的 1ms 交互式菜单（结果表格 + 固定 20/页 + `--api/--registry` 手输） | 源地址写死在命令行、分页大小写死、选中状态靠"点表格行"隐式传递 | 数据源落库（`sources` 表，`/settings` 可改）+ `search` 的多源/分页参数 + GUI 明确的选择对话框 |
| GUI 的 0.5s HTML 进度面板轮询 | 服务端轮询 + 自绘 HTML，进度口径与 CLI 不一致 | SSE `/api/events` 推送 `task.progress`/`task.updated`，CLI 与 GUI 看同一份数据 |
| Gradio 硬编码的架构候选 | 候选不来自镜像，选错只能在下载后报错 | 架构来自 manifest 的 `platforms`（`ScanPlatforms` / `--list-arch`） |
| 旧版默认关闭 TLS 校验（且无开关） | 安全默认值错误 | `verify_tls` 默认 **true**，需要时用 `--no-verify-tls` / `/settings` 关 |
| "多源 = 手选一个 host 字符串" | 无故障转移、无竞速，坏了只能改参数 | 镜像源列表落库 + 每任务可选源（`--mirror`）；**如实说明不做自动竞速**（§10） |
| 三套实现各自的版本号 | `version.txt` 三个版本号互相矛盾 | `internal/buildinfo` 单一来源 |
| `is_resume` / `retry_count` 死字段、未使用的 `get_file_size`、no-op 的 `session.timeout` | 从未生效，误导阅读 | 状态用枚举 + 回调；超时是显式参数 |

---

## 10. 后续可做

排序大致按"用户能感知的收益 / 难度"，都**还没有做**：

1. **多源并发竞速 + 自动故障转移**：现在是"选一个源"，不是"同时问几个源取快的"，也没有源挂了自动换。
   要做的话得在 `internal/registry` 之上加一层"候选源尝试器"，并把它接进 `puller` 的层下载循环（换源后 blob URL 与认证都要重算，工作量不小）。
2. **私有仓库凭据加密存储**：`-u/-p` 目前只在内存里存在一次，`/settings` 不保存 registry 凭据。
   若要保存，需要一个本机密钥（OS keychain 或 `APP_*` 提供的密钥）来加密，并明确"单用户本机"的威胁模型。
3. **镜像层跨任务去重缓存**：同一 layer 被多个镜像共用时，现在会重复下载。可做 content-addressed blob 缓存（`<dataDir>/blobs/<digest>`），
   下载前先查缓存，命中即硬链接/复制进 WorkDir。
4. **`docker load` 实测集成测试**：下载路径已在本机真实 registry 上做过端到端验证（见 §12），但那是**手工**验证。
   应在 CI 里用 Docker runner 常态化跑一次 `docker load -i` 并比对 `docker images`（需要 Docker runner，因此单列）。
5. **`artifacts` CLI 子命令**：`dockerpull artifacts list|rm|download`，让纯 CLI 用户不必开 GUI 才能管理产物。
6. **`progress.json` 增加 digest 维度**：续传前比对"账本里的层 digest"与"本次 manifest 的层 digest"，解决"同一 tag 被重推后旧账本仍被复用"的问题（§5.6）。
7. **限速与并发自适应**：`--limit-rate`，以及按失败率自动降并发。
8. **搜索源插件化**：`Searcher` 目前是内置工厂，可改成由 `sources` 表配置驱动，支持第三方搜索源。

### 明确不承诺

- 不做多用户、不做认证、不做权限（单用户本机工具，这是设计决定，不是待办）。
- 不承诺"一定有最快源"、"一定不重下"、"一定能恢复任何中断"——见 §5.6 的边界。

---

### 已修复：Windows 基础镜像的外层（foreign layer）

`hello-world:nanoserver1709` 的 manifest 把两个基础层声明为
`…rootfs.foreign.diff.tar.gzip` 并给出 `urls`（Microsoft CDN）。这类层**不存在于
registry**，必须按 `urls` 下载。原因有三处，全部修好：

1. `registry.Descriptor` 没有 `urls` 字段 → JSON 解析时就丢掉了；
2. puller 只会 `OpenBlob(ref, digest)` → 去 `/v2/.../blobs/<digest>` 取 → **403 AccessDenied**；
3. 大小兜底还会去 HEAD registry 一个它根本没有的 blob。

现在 `Descriptor.URLs` / `IsForeign()` 存在，`job.openRange` 把 foreign layer 交给
`Client.OpenBlobURLRange`，字节仍然由 `seal` 做 digest 校验，所以 CDN 无法替换内容。
`OpenBlobURLRange` **绝不携带 registry 的 Authorization**（否则把凭证泄露给第三方主机，
并且会让预签名 URL 因为多出这个头而 403）。

真实环境验证：`windows/amd64` 拉取时 `b7914a074279`(17.2MB) 与 `407ada6e90de`(77.3MB)
从 Microsoft CDN 下载成功，产出 242.9MB tar，`docker load` 报告
`os=windows arch=amd64 layers=4`。

### 已修复：标签列表的出处

inspect 之前**先问搜索源**要 tag。搜索源常常是**另一个仓库**的 API（从 ghcr.io 拉取时问的是
Docker Hub），于是同名 `owner/repo` 会把**别的发布者**的 tag 列出来，用户选了一个下载端根本没有的
tag。现在 tag 优先来自**即将拉取的那台 registry**（`GET /v2/<repo>/tags/list`），搜索源只在
registry 没有该接口时兜底，响应里的 `tagsFrom` 说明是谁回答的。`dockerpull tags` 同序。

另外 `tags/list` 是**字典序且分页**的：`library/nginx` 有几千个 tag，任何从 "1" 开始的 200 条
窗口都**不含 `latest`**，下拉框会默认停在 "1" 并且根本不提供 `latest`。现在
`Client.ensureDefaultTags` 会为用户指定的 tag 与 `latest` 各做一次 HEAD，存在的提到最前，
其余保持 registry 自己的顺序。

### 待办（设计评审提出，未实现）

来自独立的对抗性设计评审（以 AGENTS.md + CONTRACT 为准绳、用真实 registry 探测取证）。当前行为是
**诚实失败**（读不到清单就禁用下载并给出「换一个下载源」），但下面这些能把体验再抬一档：

1. `POST /api/images/inspect` 失败时返回 `suggestedHosts`（服务端算，因为只有它知道 `library/`
   规则和加速源清单），用于在界面上直接推荐可用下载源，而不是只列出全部候选。
2. `GET /api/registries` 增加命名空间标记（`namespaced` / `acceptsUnqualified`），让界面在点击**之前**
   就能提示「ghcr.io 需要 owner/repo」。
3. `default_mirror` 被设成自有命名空间的 registry（例如 `ghcr.io`）时给出警告——裸 `nginx` 会变成
   `ghcr.io/nginx`，既不等于官方 nginx，也不该补 `library/`。
4. 同名不同源的**显式提示**：quay 的 `prometheus/prometheus` 在 Docker Hub 上不存在（Hub 上是
   `prom/prometheus`）。目前是同名拉取失败即报错，不静默改映射（这是刻意的）；可以再补一句解释。
5. tar 的 `RepoTags` 对非 Hub 拉取会去掉主机名（`tar.go`），离线机器 load 后只看到 `owner/repo:tag`。
   Hub 系列正是想要这个行为；其它仓库需要先与 `docker save` 对比再决定是否改。
6. 镜像源存在滞后/缓存不全：同一个 tag 在不同源上 digest 可能不同。已记录 `task.digest`，可进一步在
   界面上展示并支持按 digest 拉取。

### 已实现：MCR 作为可搜索源

MCR 没有关键词搜索 API（其官网用的 `/api/v1/catalog/all` 裸请求返回 400），但它**匿名公开完整目录**：
`GET https://mcr.microsoft.com/v2/_catalog` → 实测 **3861 个仓库 / 约 160KB**，且该端点忽略
`n`/`last`/`query`/`search`（四个都试过，返回同一个完整列表）。所以搜索 = 拉一次目录 + 本地过滤
（多关键词需全部命中、大小写不敏感、结果排序保证翻页稳定、缓存 15 分钟）。标签走 MCR 自己的
`/v2/<repo>/tags/list`（匿名可读，实测 `dotnet/runtime` 有 8315 个 tag，`latest` 提到最前）。

实测：`search -k dotnet --source mcr` → 54 个结果（与直接探测目录得到的 54 一致）；
`pull -i mcr.microsoft.com/hello-world --mirror mcr` 成功。

这是除 Docker Hub / Quay / 1ms 之外**唯一**可搜索的上游站点。其余站点实测无匿名搜索接口
（ghcr.io `/v2/_catalog` → 401；registry.k8s.io → 404；public.ecr.aws 与 nvcr.io → 401），
只能按全称拉取。

### 已实现：`--mirror` / `registry` 接受所有常见写法

`registry.PullHost` 统一解析：加速源 id/名称/主机、上游 registry id/名称/主机、搜索源名称、
私有 host（含 `https://` 前缀与端口）。此前只解析镜像源 id，所以 `--mirror mcr` / `--mirror quay`
会被当成主机名去做 DNS 解析，报 `lookup mcr: no such host`。现在连设置里的 `default_mirror`
校验也用同一个函数，三处写法不可能不一致；既不是已知源也不像主机的裸词直接拒绝（CLI exit 2 / API 400），
不再变成一条迟到的 DNS 错误。

## 12. 真实环境验证记录

前面几节的结论大多来自**进程内假 registry**（`internal/registry/registrytest`）与结构断言。
下面这些是在本机真实 Docker 上跑通的，用来交叉验证那些"只能靠契约保证"的部分。

**环境**：Docker 29.0.1（containerd 镜像存储），本地起了一个真实 `registry:2`（`127.0.0.1:5000`，
HTTP 明文），用它作为可控的真实 registry，避开外网。

| 验证项 | 做法 | 结果 |
|---|---|---|
| 真实 registry 拉取（1 层） | 推 `alpine` 进本地 registry，`dockerpull pull -i 127.0.0.1:5000/alpine:latest --insecure` | ✅ 成功，产出 8.5MB tar |
| 真实 registry 拉取（8 层） | 同上，`nginx:alpine` | ✅ 成功，产出 61.3MB tar |
| 多架构索引解析 | 用 `docker manifest create` 在本地 registry 造出 `amd64 + arm64` 索引，`--list-arch` 与 `-a amd64/-a arm64` 各拉一次 | ✅ 列出两个架构；两次拿到的 **config digest 不同**（`320994c3…` vs `33bee74c…`），证明真的按架构选了不同 manifest |
| `docker load` | 先 `docker rmi` 删掉本地镜像，再 `docker load -i` 本项目产出的 tar | ✅ `Loaded image: nginx:alpine` / `alpine:multi` |
| `docker run` | 用 load 进来的镜像真的启动容器 | ✅ nginx 打印 `nginx/1.31.6`、`/etc/nginx/conf.d` 里能看到 `default.conf`（说明 8 层文件系统链条叠对了）；arm64 容器打印 `aarch64` |
| 内容一致性 | 对比原镜像与 load 后镜像的 `RootFS.Layers`（diff_ids）与完整 `.Config` | ✅ **逐字节等价**：8 层 diff_ids 完全相同、镜像 config 完全相同、`Created` 时间相同 |

### 一个需要说明的差异：load 后 image ID 会变

`docker load` 之后 `docker image inspect .Id` 与原镜像**不同**（例如 `df221db8…` → `86e1b0e8…`），
这不是内容被改坏——上面一行已经证明 diff_ids 与 config 完全一致。原因是：

- Docker 29 默认用 containerd 镜像存储，`docker image inspect .Id` 返回的是 **manifest 摘要**；
- 我们导出的是传统 `manifest.json` + `repositories` 归档（**有意为之**：兼容老 daemon，也和旧版 Python 工具一致），
  这个格式里没有 manifest 级摘要，containerd 载入时会**重建** OCI manifest，于是 ID 变了；
- 对照实验：用 `docker save` 导出同一镜像再 `docker load` 回来，ID **保持不变**（`df221db8…`）——
  因为那是它自己的 OCI 布局，manifest 摘要得以保留。

实际影响：按 **tag** 使用（`docker run nginx:alpine`）完全不受影响，tag 会被正确恢复；
只有"按 image ID 固定引用"的场景会看到 ID 变化。`repositories` 与 `manifest.json` 的 tag 也保证一致
（Docker Hub 官方镜像两处都写 `nginx:latest`，不会一处 `library/nginx` 一处 `nginx`）。

---

*模块路径 `github.com/Potterluo/docker-pull-tar`，二进制名 `dockerpull`；契约见 [docs/CONTRACT.md](./CONTRACT.md)。*
