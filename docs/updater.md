# 测试环境更新器

`alpha-updater` 是独立的一次性命令，用于 1.0 之前的本机测试环境升级，支持 Linux amd64/arm64。目标数据库版本见 `alpha-updater --help`，可升级范围见构建时生成的 `internal/platform/upgrade_history.json`。只更新执行命令的本机，每台 control、worker、registry、share node 分别运行。

## 获取与使用

从 v0.4.0 起，GitHub release 压缩包的 `bin/` 中包含 `alpha-updater`。v0.3.1 的包还没有此命令，可以从当前源码构建，或从目标版本包中取出；数据库超出目标版本的生成窗口时，需先使用窗口仍覆盖它的中间版本逐段升级：

```bash
# 使用完整历史和已有 tags；生成本次构建的升级窗口。
go generate ./internal/platform
go build -o bin/alpha-updater ./cmd/alpha-updater

# 查询最新正式 release，显示本机角色和安装计划；服务运行时也可查询。
./bin/alpha-updater --data-dir /var/lib/project-alpha-worker \
  --bin-dir /opt/project-alpha/bin --check

# 停止服务后更新；以拥有数据目录及二进制目录写权限的账号运行。
./bin/alpha-updater --data-dir /var/lib/project-alpha-worker \
  --bin-dir /opt/project-alpha/bin
```

更新器不会自动调用 sudo、停止或启动服务。systemd 部署先停止相应单元（例如 `project-alpha-worker.service`），完成后按原配置启动；终端运行的服务则先退出原进程。三种数据库服务必须释放数据目录的 `service.lock`，独立扫描等使用同一目录的任务也须结束。共享安装目录的所有服务都应停止。日常服务仍使用原有普通账号。

`--data-dir` 默认读取 `PROJECT_ALPHA_DATA_DIR`，否则为 `data`。`--bin-dir` 默认是当前更新器所在目录；若从临时下载目录运行，务必指定实际安装目录。只接受已有常规二进制文件；安装目标是符号链接时明确拒绝，请传入真实安装目录。

`--role` 默认 `auto`，根据数据库中保存的身份确定角色。显式指定角色时必须与数据库一致：

| 角色 | 更新文件 | 数据库 |
| --- | --- | --- |
| control | `project-alpha`、`alpha-updater` | 总控的 `platform.sqlite3` |
| worker | `project-alpha`、`rootless-docker`、`alpha-updater` | 节点的 `platform.sqlite3` |
| registry | `project-alpha`、`alpha-updater` | 注册节点的 `platform.sqlite3` |
| share-node | `project-alpha-jump`、`alpha-updater` | 无 |

share node 须显式指定角色，默认安装目录为 `/usr/local/libexec`，把发布包里的 `project-alpha` 安装为已有服务和 sshd 使用的 `project-alpha-jump`：

```bash
./bin/alpha-updater --role share-node --check
# 停止 project-alpha-share-node.service 后，以有权更新 /usr/local/libexec 的账号运行：
./bin/alpha-updater --role share-node
```

share node 更新保留账号、sshd 配置、公钥和代理配置；无需重新初始化，不创建 SSH 隧道。此角色不接受 `--data-dir` 或 `PROJECT_ALPHA_DATA_DIR`，也不访问本机平台数据库。

## Release 选择与校验

默认查询 `arusuki/alpha` 的最新正式 GitHub release。`--prerelease` 同时查询预发布，选择发布时间最新的非草稿 release；`--repo owner/repo` 可指定测试仓库。 Webhook 自动更新已从认证通知获得目标 tag 和频道，未配置 GitHub token 时直接下载 `https://github.com/<owner>/<repo>/releases/download/<tag>/project-alpha_<tag>_linux_<arch>.tar.gz` 和同目录的 `SHA256SUMS`，不查询 GitHub API。手动“更新到最新版本”和 CLI 查询仍需通过 API 确定 release；公开附件使用 `browser_download_url`。配置 token 时保留通过 release/asset API 访问私有仓库的能力。GitHub API 的行为见 [官方 release 文档](https://docs.github.com/en/rest/releases/releases)。私有仓库或需要更高 API 配额时，可在 Web 更新设置中保存 GitHub Token，或在总控服务环境中设置 `GH_TOKEN` 或 `GITHUB_TOKEN`（独立 CLI 读取自身环境）；令牌只发送给 GitHub API，不传给下载重定向的其他主机。

更新器拒绝降级、v0.3.1 之前的版本、1.0 及以上版本和无正式版本标记的本机程序。相同版本不重复更新。源码构建的更新器自身可以是 `dev`，被更新的主程序必须带 release 版本。目标包必须包含与 tag 同版本的 `project-alpha` 和 `alpha-updater`，以及根 release 附件 `SHA256SUMS`。缺少附件或角色所需程序、架构不匹配、校验失败时退出，不安装文件。

发布工作流自动将两个架构的更新器纳入包和校验文件。这些变更随 **v0.4.0** 发布；对应 GitHub release 的构建与附件上传完成后，远端更新才能获取；命令不会从 master 构建或下载未发布代码。

## 按 tag 保留最近 3 次数据库更新

升级支持窗口从最新 tag 向前数，保留最近 **3 次引入数据库 change 的 update**，以及尚未打 tag 的开发变更。计数依据是 tag 指向提交中的 `DatabaseVersion`，不是提交数、tag 总数或本机执行更新器的次数：

- 数据库版本未变化的 tag 不产生 update，也不挤占窗口。
- 一个 tag 之前的多个数据库开发版本合并算一次 update；中间未打 tag 的提交不单独计数。
- 33→34 在开发期间不占名额；实际创建 v0.4.0 tag 后，计为第 1 次已发布的数据库 update。
- 第 4 次包含数据库变更的 tag 出现时，最早一次退出支持窗口。窗口外的数据库明确报错，保留数据，需要先通过仍支持它的中间 tag 版本逐段升级。

例如带有数据库变更的 tag 是 A、B、C，期间无变更的 tag 和普通提交都不计数；窗口为 A/B/C。新的开发变更仍不挤出 A；该变更打 tag D 后，窗口才变成 B/C/D。可升级的最早数据库版本是窗口中最早一次更新的**输入版本**。

`internal/platform/upgrade_history.json` 是构建时嵌入的窗口信息，由 `go generate ./internal/platform` 从实际 Git tags 生成。发布工作流取完整 Git 历史、生成窗口，并验证发布 tag 确实指向 HEAD，二进制运行时无需 Git。浅克隆或缺少基线 tag 会明确失败；源码构建前应同步 tags 并重新生成。更新包仍按 release 下载，未发布提交不会成为远端更新目标。

`alpha-updater --database-only` 与发布更新使用的 `_migrate` 入口共用 `internal/platform/upgrade_history.go` 中的迁移链，按数据目录角色原地升级，保留已有业务数据。

新增数据库变更时，在 `internal/platform/upgrade_history.go` 登记对应版本的迁移步骤；同一 tag 内的步骤保留为完整事务链，不分别占用名额。窗口前移后可清除低于 `base_schema` 的旧步骤和实现；没有完整迁移链时构建检查或升级会报错，不跳过缺失步骤。未发布的开发迁移也必须保留数据，但不作为已发布 update 历史。

这个窗口约束的是数据库迁移支持范围；故障恢复用的本地备份沿用原来的保留方式，不按 tag 自动删除。

## 原地升级与故障恢复

下载、SHA-256 校验和可执行性检查完成后，更新器用 SQLite `VACUUM INTO` 创建 `platform.sqlite3.backup-<时间>`，包含已提交的 WAL 数据。原二进制保存在安装目录的 `.alpha-backup-*` 中。保留的数据目录、密钥、令牌、快照和实例 ID 不变；磁盘需容纳下载包、解压程序和完整数据库备份。

随后逐个原子替换程序，由**目标 release 内的更新器**继承服务锁，执行数据库迁移。迁移链在单个事务内升级，并执行完整性与外键检查。迁移错误会回滚事务，并恢复旧二进制；数据库不会被删除或自动用备份覆盖。

更新期间安装目录存在 `.alpha-update-pending`，记录备份位置和原文件是否存在。断电、强制终止、迁移结果不确定或恢复失败时保留此记录，下次更新会拒绝继续。保持服务停止，先检查数据库版本和记录：若迁移已提交，应保留目标版本二进制；若需要整体恢复，使用记录中的旧程序和数据库备份进行人工恢复，确保 SQLite 的 WAL/SHM 与数据库一致。核对程序与数据库版本匹配后，再移走恢复记录并启动服务。不要只恢复旧程序而保留新版数据库。

也可以离线仅升级本机数据库，适用于已经手动安装当前程序的情况：

```bash
./bin/alpha-updater --database-only --data-dir /var/lib/project-alpha-worker
```

该操作也校验角色、持有服务锁并先备份，使用当前执行的更新器所带迁移，不联网、不替换程序；数据库已经是当前版本时直接退出。更早或未知数据库版本明确报错并保留数据。除下文定义的稳定健康与更新管理协议外，数据库以外的配置、业务 API、快照格式不提供兼容分支；其他旧格式不匹配时按错误提示使用新数据目录。

## Web 更新设置与 GitHub webhook

总控管理员打开侧栏 **更新设置**，选择本机 control、任一 registry 或 worker。每个目标分别保存更新器命令的绝对路径、GitHub 仓库、HTTP/HTTPS 代理、预发布开关和自动更新开关。GitHub Token 在页面顶部统一配置，由总控下发给所有已登记的 registry 和 worker，不按目标独立配置。自动更新默认关闭。**立即更新到最新版本**使用已保存的设置查询 GitHub，并对所选目标执行更新；未保存的输入不会用于这次更新。

统一 GitHub Token 用于全体目标的手动和自动更新，优先于总控服务环境中的 `GH_TOKEN` / `GITHUB_TOKEN`。保存后立即向已登记节点下发，无需重启服务；新加入的节点也会收到。输入留空保留已有值，勾选清除会向节点同步清除；如果总控环境中仍有 token，则改为统一下发该值。节点的服务环境不再作为 Web 更新的独立 token 来源。下发失败时页面显示待同步节点数，通过现有 registry 连接心跳、节点后续通信和更新前补发，不增加独立定时检查；更新前下发失败会明确报错，不使用旧 token 继续启动更新。

Token 与 webhook secret 独立，仅返回是否已配置，不回显内容。总控和各节点复用项目凭据模块，以 AES-256-GCM 加密 Token；`update-settings.json` 只保存 `github_token_ciphertext`，不保存明文 Token。每个数据目录使用独立的 `update-credentials.key`，密钥文件与配置文件权限均为 `0600`，密文绑定节点角色与实例 ID。备份和恢复时须一并保留密钥；密钥缺失、权限异常或密文损坏时明确报错并保留原配置，不生成替代密钥覆盖已有密文。旧明文 Token 配置被明确拒绝，按项目约定要求使用新数据目录，不添加兼容迁移。下载时只在进程内解密并通过子进程环境传递，不写入命令参数或 `update-service.json`。磁盘加密存储不能防止同时读取密钥与配置的服务账号或 root 获取 Token。手动更新仍实时查询最新 release，不依赖通知送达；token 无效或权限不足时明确报错，不降级为匿名重试。可使用有目标仓库访问权限的 fine-grained personal access token，授予 `Contents: read`，权限要求见 [GitHub release API 文档](https://docs.github.com/en/rest/releases/releases#get-the-latest-release)。

命令字段是一个可执行文件路径，不是 shell 命令，不接受拼接参数。所有角色都以原服务账号运行更新器，不使用 sudo；账号须有数据目录及程序安装目录的写权限。服务主程序必须以 `project-alpha` 名称安装，更新文件仍写入当前主程序所在目录。HTTP 代理只用于目标主机上的 updater 下载，不改变 control/worker/registry 的通信路由。代理留空时沿用服务进程的代理环境变量。也可从命令行使用：

```bash
alpha-updater --data-dir /var/lib/project-alpha-worker \
  --http-proxy http://127.0.0.1:7890 --check
# 定向更新某个已发布版本，仍执行版本、频道及包校验：
alpha-updater --data-dir /var/lib/project-alpha-worker --tag v0.3.2
```

在更新设置中选择公网 registry，填写至少 32 字节的 Webhook Secret 并保存；Secret 不回显，留空保留，勾选清除后停用 webhook。将页面显示的 Payload URL 配置到 GitHub 仓库的 **Settings → Webhooks → Add webhook**：

- Payload URL：`https://你的-registry-域名/api/webhooks/github`，反向代理需转发此路径，registry 的 `--allowed-host` 需允许该域名。
- Content type：`application/json`。
- Secret：与 registry 保存的值一致。
- Events：选择 **Releases**，启用 webhook。

registry 按原始请求体校验 `X-Hub-Signature-256` 的 HMAC-SHA256 签名，仅处理配置仓库的 `release / published`；支持 GitHub 的 `ping`。签名规则参见 [GitHub 官方文档](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)，事件定义参见 [release webhook 文档](https://docs.github.com/en/webhooks/webhook-events-and-payloads#release)。草稿、编辑等事件不会启动更新。

通知先持久化到 registry，再通过 control 主动建立的现有 WebSocket 发送给 control，由 control 通知全部已登记 worker。沿用已有连接心跳重试，不增加独立的健康检查任务。worker 离线时通知保持待确认，重连后再次发送；节点保存新版本通知、过滤自己的预发布频道并去重。registry 也会转发预发布通知，即使它自己仅使用正式版。registry 最多保留 128 条待发送通知和最近 256 个 delivery ID；队列满时返回 503，不丢弃已有通知。重复通知不会重复启动同一 tag 的自动更新。下游持续离线或仓库设置不一致会阻止队列确认，可在 registry 更新设置中查看待确认数量与错误。

worker 可在接受通知后按配置自动更新；control 等该通知完成 worker 投递后才自动更新，registry 等通知队列确认完毕才自动更新。所有目标使用同一个发布仓库；各自的自动更新和预发布开关独立设置。自动更新失败不会反复重启同一版本，请查看错误并手动重试。GitHub 不会自动重投失败的 webhook 请求；若 registry 当时不可达或队列已满，请在 GitHub Recent deliveries 中重投。

Web 更新通过本地服务交接实现，要求主程序和 updater 同时更新到支持本地后台下载协议 v2 的版本；协议不匹配时明确报错，服务继续运行。接受请求后以原服务账号启动 updater 的后台准备进程，页面显示“正在后台下载并校验”，HTTP 和业务工作继续运行。准备阶段只读数据库身份和版本，不获取服务锁、不备份或迁移数据库、不替换运行中的程序；下载、SHA-256 校验、解压、版本及可执行性检查完成后保存本地安装计划。下载失败或已经是最新版本时不重启；失败结果可在页面查看并手动重试，同一 tag 不自动反复尝试。服务正常退出时取消并等待准备进程结束。

只有准备成功才停止 HTTP 服务并结束后台工作、释放数据库连接与服务锁，原进程再 `exec` 为 updater。安装阶段完全离线，重新获取安装锁和服务锁，校验本机版本、数据库版本和暂存程序哈希，然后备份数据库、替换程序并原地升级；备份包含下载期间提交的新数据。更新完成后以原来的启动参数、环境和工作目录 `exec` 回 `project-alpha`，保持同一 PID，适用于普通终端及 systemd。常规命令行更新仍要求先停服务，不自行管理服务。失败且安装状态确定时重启原程序并显示失败；存在 `.alpha-update-pending` 时拒绝重启，服务启动入口也拒绝带此标记启动，须先人工核对恢复。

GitHub 的 403 不一定是限流。错误现在保留 GitHub 返回的 message、可用的 `X-RateLimit-*` / `Retry-After` 信息和是否携带认证的状态，不输出 token。只有响应提供限流证据时才标为限流，不会立即循环重试。未认证 REST API 请求按出口 IP 共享每小时 60 次额度，多台节点共用代理或 NAT 时会共用额度；如果仍需 API 查询，可在 Web 更新设置中保存统一 GitHub Token，或在总控服务环境中配置 `GH_TOKEN` 或 `GITHUB_TOKEN`，仅在交互 shell 中设置不会修改已运行的服务环境。详见 [GitHub 限流说明](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)。公开仓库的 webhook 自动更新下载路径不使用这项 REST API 额度。

设置及通知保存在数据目录的 `update-settings.json`，交接参数为 `update-service.json`，最近结果为 `update-result.json`，完整输出追加到 `update.log`，均由原服务账号持有，新建文件权限为 0600。设置格式错误会明确报错并保留原文件。本功能没有新增数据库表，已有数据库继续使用 updater 的原地升级流程。

## 跨版本的健康与更新约定

从支持本功能的首个版本起，`/api/management/v1/{health,settings,update,release}` 和 `alpha-management-v1` WebSocket 信封是稳定的管理协议；后续业务 API 和快照版本变化不应改变它。JSON 接收端容忍附加字段，身份与认证字段保持不变。worker 使用既有 Bearer token、`X-Alpha-Node` 与 `X-Alpha-User`（id/username/role）；registry 管理接口额外校验已绑定的 `X-Alpha-Control`。浏览器仅由总控的管理员会话及 CSRF 校验进入 `/api/updates/`，节点令牌不会发给浏览器。统一 token 的管理员配置入口为 `/api/updates/github-token`，节点下发入口为 `PUT /api/management/v1/github-token`，沿用同一身份校验；需要总控及节点程序均支持该入口。

`/api/worker/info`、`/api/registry/info` 的身份、`protocol`、`management_protocol` 和 `version` 字段作为稳定发现信息保留。业务协议不同的节点可登记、显示在线并更新；详情接口返回明确的 409，容器汇总标记不完整。registry 的 `ping` 和 `release.v1` 不受注册业务协议版本限制，业务请求仍校验自己的 protocol。

此约定不能让已经发布、尚未实现这些接口的旧程序自动获得支持。**首次部署需要先在各主机安装支持管理协议和服务交接的 project-alpha / alpha-updater，再使用网页更新与 webhook。** 旧版 registry 的 WebSocket 协议也需完成这次引导升级。之后各角色可分批更新，业务详情可暂时不可用，健康和更新通道保持可用。
