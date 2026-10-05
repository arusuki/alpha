# 跳板机与使用者资源

总控维护 Tailscale 分享节点池。成员通过分配到的 share node 访问计算节点，并通过该节点的 HTTP 代理访问总控 status 页面。公钥保存在 share node，不要求总控本机安装跳板账号。

完整的部署步骤、总控服务账号 SSH 配置、主机公钥信任和排错见 [share node 配置指南](share-node.md)。

## 配置

1. 总控以普通服务用户运行；让 Web 服务监听 share node 可达的内网地址，例如 `--host 10.0.0.1 --port 8765 --allowed-host 10.0.0.1`。总控仅监听 `127.0.0.1` 时，其他主机上的代理无法连接；直接访问总控 IP 或域名需通过 `--allowed-host` 允许。
2. 在总控「Share node 管理 → 全局连接设置 → 总控 SSH 密钥」点选已创建的密钥，没有密钥时先创建并下载公钥；授权后校验并启用，用于管理连接。在 share node 安装 OpenSSH 和 systemd，管理公钥可在初始化时提供，也可稍后追加；加入分享池前需授权总控 SSH **公钥**，私钥始终保留在总控。
3. 首次连接自动将 share node 的 SSH 主机公钥记录到总控服务用户的 `known_hosts`，已记录的主机公钥变化时拒绝连接。网页保存 Tailscale API Key（`tskey-api-…`）和 Tailnet，查询已授权的自有节点，配置 SSH 端口和网页入口端口后加入分享池。加入和重新启用时，通过 `alpha-worker` 校验免密登录、管理协议、监听 IP 和入口端口；失败不保存节点。
4. 在总控为 worker 配置内网 IP，在 worker 的容器管理中配置默认镜像、数据目录、Docker endpoint 和起始端口。share node 需能访问这些计算节点的容器 SSH 端口。
5. 生成 [注册邀请码](members.md)，通过 API 或 [公网 registry](operations.md#公网-registry) 注册。按关联人数最少优先分配分享节点，停用只影响新分配。

## 一次初始化与卸载

「总控 SSH 密钥」使用卡片选择，仅列出数据目录内创建的密钥；不提供手填路径、服务用户 `~/.ssh` 文件扫描或默认身份选项。接口同样拒绝空身份、外部文件和符号链接。私钥必须属于服务用户，权限为 `0600` 或 `0400`；公钥及 SHA256 指纹从私钥派生，避免使用不匹配的 `.pub` 文件。

连接使用 `ssh -F /dev/null -i <路径> -o IdentitiesOnly=yes`，仅使用已启用的密钥。主机信任使用 `StrictHostKeyChecking=accept-new` 和 OpenSSH 默认的 known_hosts 路径，服务账号须有写入权限；尚未启用密钥时提示先选择或创建。

生成操作只创建候选密钥，不切换当前身份。新密钥保存在 control 数据目录的 `control-ssh/<名称>-<随机目录>/id_ed25519`，目录 `0700`、私钥 `0600`，不会覆盖已有密钥；下载仅提供公钥。点击「校验并启用所选密钥」后持久生效，无需重启。更换身份前先在全部 share node 运行 `share-node --add-control-file <候选公钥文件>` 追加授权；保存时对所有已配置节点（包括停用节点）执行只读 `inspect`，任一 SSH 或协议校验失败均保留原配置。不要在新身份验证成功前撤销旧公钥；追加授权和重复初始化均保留原有管理公钥。

主控 SSH 配置使用版本号防止覆盖他人修改，仅管理员可读取或更改。新增持久配置使用数据库格式 32；旧数据目录会明确报版本不匹配，按项目约定需使用新数据目录，程序不会自动迁移或删除旧数据。

在 **share node 本机** 执行：

```sh
sudo ./project-alpha share-node \
  --listen-host 100.64.0.2 \
  --control-url http://10.0.0.1:8765 \
  --status-port 9765
```

管理公钥现在选填。可在初始化命令中加 `--control-key-file /tmp/control-service.pub`，也可初始化后单独追加：

```sh
sudo ./project-alpha share-node --add-control-file /tmp/control-service.pub
# 或直接传入一行公钥（替换为实际内容）
sudo ./project-alpha share-node --add-control-key 'ssh-ed25519 AAAA... control-service'
```

`--add-control-key` 和 `--add-control-file` 可在初始化时一起使用，也可单独运行、重复传入；已有授权会保留，重复公钥自动忽略。文件中需为一行公钥。追加需要 sudo，无需重启服务；不接受成员公钥池中已有的公钥。未提供管理公钥时代理仍可启动，但 `alpha-worker` 暂不能登录，需先追加授权再加入分享池。

这个子命令创建两个独立的系统账号、安装公钥工具、配置 sshd 并启用 `project-alpha-share-node.service`。默认入口端口为 8765，监听地址需为 share node 自己的 Tailscale IP；代理直接连接指定总控 HTTP/HTTPS 地址，不建立 SSH 隧道。

| 账号 | 职责与权限 |
| --- | --- |
| `alpha-worker` | 接受总控管理公钥；SSH 仅允许固定 `alpha-worker cmd` 公钥管理协议，禁止任意 shell、SFTP、PTY 和所有转发；HTTP 代理服务也以此账号运行 |
| `alpha-jump` | 接受成员公钥；只允许 ProxyJump 所需的本地 TCP 转发，禁止命令、shell、SFTP、PTY、反向监听、Agent、X11 和 Unix socket 转发 |

创建账号、修改 sshd、安装服务和追加管理公钥需要本机 sudo。日常总控、代理、SSH 内部管理命令和公钥读取器均不使用 root 账号。总控使用已创建并启用的 SSH 密钥，强制公钥认证、严格主机公钥校验，并关闭客户端额外命令、转发和共享 SSH 会话。

`--control-url` 是 share node 能直接访问的总控地址，不能包含账号、路径、查询参数或片段。Go 标准库反向代理保留请求路径、查询、浏览器 Host 和 Origin，转发到固定目标；总控继续校验成员令牌、管理员会话及 CSRF。入口使用 HTTP，数据通过 Tailscale 网络传送。更改总控地址后，在 share node 重复初始化更新代理配置，并在总控重新保存分享节点以更新记录的代理目标。

重复初始化保留账号身份、成员公钥和已有管理公钥，并合并本次提供的管理公钥；已有未管理账号、旧安装格式或冲突的 SSH 配置会明确拒绝，不自动接管或覆盖。配置经过 `sshd -t` 和两个账号的 `sshd -T -C` 校验，安装失败恢复本次修改的配置；原始 SSH 配置备份保留在 `.before-alpha-share-node`。

非 systemd 部署可使用 `--no-reload --no-service`，自行重载 sshd 并以 `alpha-worker` 运行 `/usr/local/libexec/project-alpha-jump share-node --serve`。默认部署无需这些参数。

卸载也是同一个子命令：

```sh
sudo ./project-alpha share-node --uninstall
```

卸载停止并移除代理服务，撤销本工具的 SSH 配置块，删除两个专用账号、组、工具、安装记录及成员公钥数据，保留原始 sshd 备份和其他账号的 SSH 配置。先在总控回收成员并移除池配置；卸载检查账号身份和活动进程，有管理或成员 SSH 连接时拒绝删除，不强制终止。重试卸载可继续处理已删除的账号。非 systemd 部署先停止手动代理，再加 `--no-reload --no-service`。

安装记录采用格式 **3**，将管理公钥保存在必填的 `control_keys` 列表中，空列表表示尚未授权总控。旧记录不迁移，格式不匹配时需使用新的安装数据目录；成员公钥清单和管理协议仍为版本 2。

安装记录 `/var/lib/project-alpha-jump/installation.json`、管理公钥授权文件和 `/usr/local/libexec/project-alpha-jump` 由 root 持有。`keys/` 属于 `alpha-worker`，组为 `alpha-jump`，权限 `2750`；清单 `keys/keys.json` 为 `0640`，成员账号只能读取。公钥发布通过文件锁和原子替换，拒绝无效身份、格式、公钥、权限或链接文件。管理授权独立于成员公钥池，成员撤销不会切断总控管理连接。

## 公钥池

“Share node 管理”按节点展示独立卡片，集中显示连接信息、`alpha-worker` / `alpha-jump` 账号用途、本节点公钥池和使用者。同时保留“按使用者查看资源”列表，汇总全部使用者及其所属 share node，并标明尚未分配或节点不可用的情况；总控 SSH 身份与 Tailscale 凭据位于可展开的“全局连接设置”。

公钥只发布到成员实际分配的 share node。每个节点的条目按规范化公钥关联成员，ID 独立于成员 ID；多名成员可以共用公钥。撤销同一节点上的最后一个引用成员时删除对应公钥的全部条目，不影响其他 share node。撤销只影响后续认证。

`used` 表示该节点上有成员引用，`free` 表示没有；**free 公钥仍可用于 SSH 认证**。管理员可以按节点清理 free 条目，服务端在事务中复核引用，已被引用时返回 409。「补齐全部节点公钥」按分配节点补齐活跃成员的公钥，保留 free 条目；离线节点报告错误，其他节点仍可显示和管理。

容器公钥写入所分配容器的 root `authorized_keys`，容器可使用正常 shell。成员 SSH config 的 `alpha-jump` HostName 和端口来自分配到的 share node，容器目标使用计算节点内网 IP 及分配端口。

## 注册与补申请

注册事务提交邀请码扣减、成员、资源令牌摘要和资源任务，随后后台分配。`201` 仅表示登记成功；单项失败保留错误及已成功资源，不重复扣名额。

调用本人 API 携带 `Authorization: Bearer <resource_token>`，无需平台 Cookie 或 CSRF，仍校验 Host/Origin。令牌只能访问本人的资源，丢失后由管理员重置。总控只保存 SHA-256 摘要。

| 方法 | 用户 API | 请求 / 返回 |
| --- | --- | --- |
| GET | `/api/members/me/resources` | 本人 `access` 和全部当前 node；包括尚未申请的 node |
| POST | `/api/members/me/containers` | `{"node_id":"…"}`；探测在线后立即创建，成功返回 200 和已就绪资源，失败返回具体错误；支持新添加的 node |
| POST | `/api/members/me/retry` | `{}`；重试失败的跳板公钥写入和 node，返回 202 |
| GET | `/api/status/<username>` | 本人状态页数据；全部 node 的基本信息、在线状态、容器总数及仅属于本人的容器 |
| POST | `/api/status/<username>/containers` | `{"node_id":"…"}`；与本人容器申请接口共用创建流程，URL 用户名必须与令牌所属使用者一致 |

`control` 返回通过 share node 代理的本人 `status_url`；未分配分享节点时 URL 为空。`access.share_host/share_ssh_port/status_port` 为成员入口，`nodes[].internal_ip` 取自计算节点配置。

`access` 返回分享节点、邀请链接及状态、公钥状态和错误；`nodes` 返回各 worker 的分配状态、容器 ID/名称、端口及总控配置的计算节点内网 IP（`internal_ip`），不返回服务凭据或 root 密码。

总控 `/status/<username>` 页面（如 `/status/alice`）凭本人令牌查看节点及容器，并申请尚未分配的在线节点；`username` 是注册时的唯一使用者标识。页面同时给出 SSH config 示例：分配的 share node IP 和 SSH 端口用于固定 `alpha-jump`，计算节点 IP 和分配端口用于容器，通过 `ProxyJump alpha-jump` 连接；私钥路径应指向注册公钥对应的本机私钥。尚无容器时先申请，成功后配置自动更新。令牌仅存当前标签页的 `sessionStorage`，退出清除。离线节点保留中央分配记录，运行状态来自最近采集。

每个 `(member_id,node_id)` 只有一个分配槽；worker 保存创建计划，断线或重启后核对同一容器继续。重复申请返回现有分配，外部容器或未标记的数据目录冲突时拒绝。状态为 `unallocated/pending/running/ready/failed/deleting/deleted`；失败须显式重试，同一成员正在处理时返回 409，后台最多并发 8 个 worker。

## Tailscale 分享与核对

每名成员得到一个单次节点分享链接，用自己的 Tailscale 账号接受；管理员可刷新接受状态。API Key 使用 AES-GCM 加密，密钥为 control 数据目录的 `tailscale-api-token.key`（0600），缺失或损坏时报错。保存配置须带 revision；已有分享池时不能切换 Tailnet 或清除凭据，可替换到期 Key。

创建邀请结果明确被拒绝时可重试；断线、5xx、无效响应或持久化中断时保留 `unknown/creating`，避免重复分享。管理员核对节点邀请列表，关联本次新增的单次邀请 ID；不能关联旧邀请或其他成员已用邀请。确认没有新增邀请或已手动撤销后，可提交 `confirm_absent:true`，服务端再次核对列表后才允许重试。

## 回收与管理接口

删除成员先标记 `deleting`，停止本人令牌访问和新申请，再撤销 Tailscale 分享和跳板公钥，清空各 worker 上该使用者的容器归属。容器保持当前运行状态，管理记录、workspace/home 和共享 `/data` 均保留，显示为未归属，等待管理员手动回收。成功返回前直接删除成员及资源记录，保留审计和邀请码累计用量；失败时返回具体错误，管理员可重试删除。Tailscale 撤销返回 400 时再次按邀请 ID 查询，仅确认邀请不存在（404）后视为已撤销，权限、网络及其他错误保留。

保留容器内的原公钥也不会自动删除。容器直接可达或经其他跳板可达时，原私钥仍可能登录；管理员手动回收时需清理容器授权或停用、删除容器，见 [公钥与访问边界](share-node.md#公钥与访问边界)。

离线、失败或结果未知时保留记录，修复后再次 `DELETE` 继续；仍有资源引用的 worker 或分享节点不能移除。worker 保留已删除成员 ID 的分配槽，阻止迟到请求重建资源。

所有管理写操作要求平台管理员会话和 `X-CSRF-Token`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / PUT | `/api/bastion/ssh` | 读取总控 SSH 密钥、候选路径、公钥和指纹；保存 `{revision,identity_file}`，仅接受已创建的密钥路径；切换前验证全部分享节点 |
| GET | `/api/bastion/ssh/public-key?identity_file=…` | 查看候选私钥对应的公钥与指纹；不会返回私钥 |
| POST | `/api/bastion/ssh/generate` | `{name}` 生成新的 Ed25519 候选密钥，返回路径、公钥与指纹；不自动保存身份 |
| DELETE | `/api/bastion/ssh/identity` | `{identity_file}` 删除 control-ssh 中未启用的密钥文件；拒绝删除当前身份、外部文件和符号链接，记录审计 |
| GET | `/api/bastion/resources` | 分享池的 SSH 地址、网页入口和总控代理目标，成员分配及远程 `key_pool`；离线池保留错误 |
| GET | `/api/bastion/keys` | 远程公钥池条目，含 `node_id/node_name`、指纹、关联成员和 `used/free` 状态 |
| POST | `/api/bastion/keys/sync` | `{}`；按成员分配节点补齐公钥，保留 free 条目 |
| DELETE | `/api/bastion/keys/<nodeId>/<entryId>` | `{}`；清理指定 share node 的 free 公钥，已引用时返回 409 |
| GET / PUT | `/api/tailscale/settings` | `{revision,tailnet,api_token}` 配置；读取不返回 Key |
| POST | `/api/tailscale/test` | `{}` 测试已保存凭据 |
| GET | `/api/tailscale/devices` | 读取当前网络设备 |
| PUT | `/api/bastion/tailscale/<nodeId>` | `{enabled:true/false,ssh_host,ssh_port,status_port}`；加入或启用前校验 SSH，端口默认 22 / 8765，IP 必须属于该 Tailscale 节点；有成员引用时禁止改地址 |
| GET | `/api/bastion/tailscale/<nodeId>/invites` | 核对节点现有邀请 |
| DELETE | `/api/bastion/tailscale/<nodeId>` | 无使用者引用时移除分享池配置 |
| POST | `/api/bastion/members/<id>/refresh` | `{}` 同步已知邀请接受状态 |
| POST | `/api/bastion/members/<id>/resolve-invite` | `{invite_id:"…"}` 关联结果未知的邀请，或 `{confirm_absent:true}` 确认无残留 |
| GET | `/api/members/<id>/resources` | 管理员查询资源 |
| POST | `/api/members/<id>/retry` | `{}` 重试资源分配 |
| POST | `/api/members/<id>/containers` | `{node_id:"…"}` 立即申请指定 node，成功返回 200，失败返回错误 |
| POST | `/api/members/<id>/token` | `{}` 重置并返回一次新的 `resource_token` |
| DELETE | `/api/members/<id>` | `{}` 撤销分享、公钥并清空容器归属后删除使用者；成功返回 200 |

worker 内部接口为 `PUT/DELETE /api/containers/members/<member_id>`，由总控后台使用服务凭据调用，不经浏览器节点代理开放。
