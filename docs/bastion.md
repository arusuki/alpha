# 跳板机与使用者资源

总控维护 Tailscale 分享节点池。成员通过分配到的 share node 访问计算节点，并通过该节点的 HTTP 代理访问总控 status 页面。公钥保存在 share node，不要求总控本机安装跳板账号。

完整的部署步骤、总控服务账号 SSH 配置、主机公钥信任和排错见 [share node 配置指南](share-node.md)。

## 公钥池

成员注册时按关联人数最少优先分配分享节点，停用节点只影响新分配。

“Share node 管理”按节点展示独立卡片，集中显示连接信息、`alpha-worker` / `alpha-jump` 账号用途、本节点公钥池和使用者。同时保留“按使用者查看资源”列表，汇总全部使用者及其所属 share node，并标明尚未分配或节点不可用的情况；总控 SSH 身份与 Tailscale 凭据位于可展开的“全局连接设置”。

公钥只发布到成员实际分配的 share node。每个节点的条目按规范化公钥关联成员，ID 独立于成员 ID；多名成员可以共用公钥。撤销同一节点上的最后一个引用成员时删除对应公钥的全部条目，不影响其他 share node。撤销只影响后续认证。

`used` 表示该节点上有成员引用，`free` 表示没有；**free 公钥仍可用于 SSH 认证**。管理员可以按节点清理 free 条目，服务端在事务中复核引用，已被引用时返回 409。「补齐全部节点公钥」按分配节点补齐活跃成员的公钥，保留 free 条目；离线节点报告错误，其他节点仍可显示和管理。

容器公钥写入所分配容器的 root `authorized_keys`，容器可使用正常 shell。成员 SSH config 的 `alpha-jump` HostName 和端口来自分配到的 share node，容器目标使用计算节点内网 IP 及分配端口。

## 注册与补申请

注册事务提交邀请码扣减、成员密码摘要与密文、资源令牌摘要和资源任务，随后后台分配。`201` 仅表示登记成功；单项失败保留错误及已成功资源，不重复扣名额。

自动化客户端调用 `/api/members/me/*` 携带 `Authorization: Bearer <resource_token>`，无需平台 Cookie 或 CSRF，仍校验 Host/Origin。令牌只能访问本人的资源，丢失后由管理员重置。总控只保存 SHA-256 摘要。

| 方法 | 用户 API | 请求 / 返回 |
| --- | --- | --- |
| GET | `/api/members/me/resources` | 本人 `access` 和全部当前 node；包括尚未申请的 node |
| POST | `/api/members/me/containers` | `{"node_id":"…","mode":"create","container_id":""}`；探测在线后执行所选分配，成功返回 200 和已就绪资源，失败返回具体错误；支持新添加的 node |
| POST | `/api/members/me/retry` | `{}`；重试失败的跳板公钥写入和 node，返回 202 |
| POST | `/api/status/<username>/login` | `{"password":"…"}`；返回 `session_token`，仅能用于此使用者的 status API |
| POST | `/api/status/<username>/logout` | 撤销当前登录会话 |
| POST | `/api/status/<username>/keys` | `{"ssh_public_key":"公钥一\n公钥二"}`；保存公钥列表并自动下发，返回 202；失败后再次保存可重试 |
| POST | `/api/status/<username>/password` | `{"current_password":"…","password":"…"}`；修改密码并撤销全部状态页会话 |
| GET | `/api/status/<username>` | 本人状态页数据；全部 node 的基本信息、在线状态、容器总数、本人容器及领养候选列表（已领养项只读显示） |
| GET | `/api/status/<username>/disk?node_id=…` | 最近完成扫描的分区容量与容器用量概览；附加 `container` 查看本人容器来源，附加 `path`、`offset` 分页读取其已采集目录明细（每页 50 项） |
| POST | `/api/status/<username>/containers` | `{"node_id":"…","mode":"create","container_id":""}`；与本人容器申请接口共用领养或新建流程，URL 用户名必须与登录会话所属使用者一致 |

`/api/status/*` 除登录外携带 `Authorization: Bearer <session_token>`；不接受资源令牌。修改密码要求当前密码正确。密码长度和存储见 [使用者登记](members.md)。新容器与创建失败后尚未初始化的容器使用当前密码完成初始化；已经初始化的容器不会重置。

`control` 返回通过 share node 代理的本人 `status_url`；未分配分享节点时 URL 为空。`access.share_host/share_ssh_port/status_port` 为成员入口，`nodes[].internal_ip` 取自计算节点配置。

status 顶层 `ssh_public_key` 返回本人公钥列表（一行一个），`nodes[].key_state/key_error` 返回最近公钥修改的同步状态和错误。`access` 返回分享节点、邀请链接及状态、公钥状态和错误；`nodes` 返回各 worker 的分配方式（`mode`）、领养目标（`target_id`）、分配状态、容器 ID/名称、端口及总控配置的计算节点内网 IP（`internal_ip`），不返回服务凭据或 root 密码。

状态页的磁盘面板与 GPU 面板独立开关、选择节点和刷新，显示分区已用、总量、可用空间，以及容器独占和共享用量。容器明细默认收起，只允许本人展开；展开后可选择可写层或挂载来源，通过交互式容量图点击目录逐层下钻，使用面包屑返回，并点击文件查看用量。分页外的目录项和目录自身及未展开空间单独标注，零占用或未知项保留在列表中。其他人的容器仅显示用量摘要。服务端逐次校验容器归属和目录范围，不下发宿主机目录树或其他容器的来源路径；宿主机分区入口挂载不能用于展开全盘。磁盘数据为扫描观察值，刷新不会触发扫描；缺少明细时需等待管理员补充扫描，共享空间和可写层不能重复相加。总控、worker 和 share node 代理需更新后使用此入口。

总控 `/status/<username>` 页面（如 `/status/alice`）用注册时设置的密码登录后查看节点及容器，并申请尚未分配的在线节点；`username` 是注册时的唯一使用者标识。页面同时给出 SSH config 示例：分配的 share node IP 和 SSH 端口用于固定 `alpha-jump`，计算节点 IP 和分配端口用于容器，通过 `ProxyJump alpha-jump` 连接；私钥路径应指向注册公钥对应的本机私钥。尚无容器时先申请，成功后配置自动更新。登录会话仅存当前标签页的 `sessionStorage`，12 小时后过期，退出时同时撤销服务端会话；浏览器不保存密码。离线节点保留中央分配记录，运行状态来自最近采集。

每个 `(member_id,node_id)` 只有一个分配槽。注册时必须逐节点选择 `create` 或 `adopt`；补申请提交 `{node_id,mode,container_id}`，新建的 `container_id` 为空，领养为完整容器 ID。worker 保存选择和创建计划，断线或重启后核对同一容器继续；重试不允许更换原选择。重复同一申请返回现有分配，新建遇到已有归属或未标记的数据目录冲突时拒绝，领养则复用现有容器并添加成员公钥，保留密码和数据。已领养项仍显示在选择列表中，但置灰禁用。状态为 `unallocated/pending/running/ready/failed/deleting/deleted`；失败须显式重试，同一成员正在处理时返回 409，后台最多并发 8 个 worker。

## Tailscale 分享与核对

每名成员得到一个单次节点分享链接，用自己的 Tailscale 账号接受；管理员可刷新接受状态。API Key 使用 AES-GCM 加密，密钥为 control 数据目录的 `tailscale-api-token.key`（0600），缺失或损坏时报错。保存配置须带 revision；已有分享池时不能切换 Tailnet 或清除凭据，可替换到期 Key。

创建邀请结果明确被拒绝时可重试；断线、5xx、无效响应或持久化中断时保留 `unknown/creating`，避免重复分享。管理员核对节点邀请列表，关联本次新增的单次邀请 ID；不能关联旧邀请或其他成员已用邀请。确认没有新增邀请或已手动撤销后，可提交 `confirm_absent:true`，服务端再次核对列表后才允许重试。

## 回收与管理接口

删除成员先标记 `deleting`，停止本人令牌、登录会话访问和新申请，再撤销 Tailscale 分享和跳板公钥，先撤销各 worker 容器内带该成员标记的全部公钥，再清空该使用者的容器归属。容器保持当前运行状态，管理记录、workspace/home 和共享 `/data` 均保留，显示为未归属，等待管理员手动回收。成功返回前直接删除成员及资源记录，保留审计和邀请码累计用量；失败时返回具体错误，管理员可重试删除。Tailscale 撤销返回 400 时再次按邀请 ID 查询，仅确认邀请不存在（404）后视为已撤销，权限、网络及其他错误保留。

平台管理的成员公钥自动清理，容器内无关或手工添加的授权保留。节点不可达、容器停止或清理失败时保留归属，修复后重试删除。撤销不终止现有 SSH 连接，见 [公钥与访问边界](share-node.md#公钥与访问边界)。

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
| PUT | `/api/bastion/tailscale/<nodeId>` | `{enabled:true/false,ssh_host,ssh_port}`；加入、保存配置或启用前校验 SSH，SSH 端口默认 22，IP 必须属于该 Tailscale 节点；网页入口端口和总控代理地址自动读取，不接受手填；有成员引用时禁止改 SSH 或入口地址 |
| GET | `/api/bastion/tailscale/<nodeId>/invites` | 核对节点现有邀请 |
| DELETE | `/api/bastion/tailscale/<nodeId>` | 无使用者引用时移除分享池配置 |
| POST | `/api/bastion/members/<id>/refresh` | `{}` 同步已知邀请接受状态 |
| POST | `/api/bastion/members/<id>/resolve-invite` | `{invite_id:"…"}` 关联结果未知的邀请，或 `{confirm_absent:true}` 确认无残留 |
| GET | `/api/members/<id>/resources` | 管理员查询资源 |
| POST | `/api/members/<id>/retry` | `{}` 重试资源分配 |
| POST | `/api/members/<id>/containers` | `{node_id:"…",mode:"create",container_id:""}` 立即申请指定 node，成功返回 200，失败返回错误 |
| POST | `/api/members/<id>/token` | `{}` 重置并返回一次新的 `resource_token` |
| DELETE | `/api/members/<id>` | `{}` 撤销分享、公钥并清空容器归属后删除使用者；成功返回 200 |

worker 内部接口为 `PUT/DELETE /api/containers/members/<member_id>`，由总控后台使用服务凭据调用，不经浏览器节点代理开放。
