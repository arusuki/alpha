# 跳板机与使用者资源

总控「跳板机管理」维护 Tailscale 分享节点池和固定 `alpha-jump` 账号。注册时发布成员公钥并分配容器，删除成员时回收。数据库格式及旧数据处理见 [README](../README.md#配置与数据)。

## 配置

1. 保存 Tailscale API Key（`tskey-api-…`）和 Tailnet（`-` 为凭据所属网络），从已授权的自有节点中选择分享节点。总控无需 Tailscale 客户端；按关联人数最少优先分配，停用只影响新分配。
2. 安装 OpenSSH，以普通系统用户运行 control，在网页添加或接管 `alpha-jump`。安装时使用页面所示服务用户的 sudo 密码，用户和数据目录由服务端确定。
3. 在每个 worker 的容器管理中配置镜像、数据目录、Docker endpoint、起始端口及 SSH 地址。注册容器使用默认镜像、bridge 网络、全部 GPU 和自动端口。
4. 生成 [注册邀请码](members.md)，通过 API 或 [公网 registry](../README.md#公网-registry) 注册。

## 跳板账号

服务用户须能通过 sudo 执行当前二进制的 `bastion install-helper`。systemd 的 `User=` 与服务用户一致；网页账号操作要求 `NoNewPrivileges=false`，见 [部署单元](../deploy/project-alpha.service)。日常公钥管理无需 sudo。

每次账号操作使用本次输入的密码，不缓存或持久化。密码经 stdin 管道交给 sudo，不传给安装辅助程序；关闭弹窗或断开请求会取消安装。认证最多等待 30 秒，整次请求最多 45 秒；中断后刷新状态核对。

| 操作 | 行为 |
| --- | --- |
| 添加 / 重装 | 创建固定账号，或更新当前接管账号的工具与 SSH 配置，保留公钥 |
| 接管 | 保留账号 UID/GID、home 及全部公钥，将管理权交给当前 control / 服务用户，并补齐活跃成员缺少的公钥 |
| 取消接管 | 停止公钥写入，保留账号、工具、数据和已有 SSH 授权；重新接管后可重试待处理项 |
| 删除账号 | 确认 `alpha-jump`，要求公钥池为空；不强制终止进程，保留工具、数据、home 和 SSH 配置 |

终端命令以 `yuuka` 为服务用户示例：

```sh
sudo ./bin/project-alpha bastion init --service-user yuuka --data-dir /var/lib/project-alpha-control
sudo ./bin/project-alpha bastion adopt --service-user yuuka --data-dir /var/lib/project-alpha-control
sudo ./bin/project-alpha bastion release --service-user yuuka --data-dir /var/lib/project-alpha-control
sudo ./bin/project-alpha bastion delete --service-user yuuka --data-dir /var/lib/project-alpha-control --confirm alpha-jump
```

数据目录上级须存在，已有目录须属于服务用户且不可由其他用户写入；数据库由服务用户初始化。`init` 不会隐式接管已有账号或其他 control 的安装。`adopt` 可接管已离开的服务用户所留下的安装，原目录归档到 `/var/lib/project-alpha-jump.before-adopt-*`。删除后重新添加复用原 UID/GID；身份冲突或格式损坏时拒绝操作。

`init` / `adopt` 在 `/etc/ssh/sshd_config` 末尾维护固定账号的规则，经 `sshd -t` 和 `sshd -T -C` 校验后重载；发现已有规则冲突时拒绝，重载失败恢复原配置。首次改动备份为 `.before-alpha-jump`。自行管理 sshd 时可加 `--no-reload`，之后须自行重载。`release` / `delete` 不改 SSH 配置。

新账号 home 为 `/var/empty/alpha-jump`，shell 为 `/bin/false`。SSH 只允许公钥认证和 ProxyJump 所需的本地 TCP 转发，禁止 shell、SFTP、PTY、Agent、X11 和 Unix socket 转发。使用 `AuthorizedKeysFile none`，home 中已有的 `authorized_keys` 不参与认证。读取器以 `alpha-jump` 运行，独立于 control 主进程。

安装记录 `/var/lib/project-alpha-jump/installation.json` 和读取器 `/usr/local/libexec/project-alpha-jump` 由 root 持有。`keys/` 由服务用户持有，组为 `alpha-jump`，权限 `2750`；清单 `keys/keys.json` 为 `0640`。发布使用文件锁及原子替换，账号操作与发布串行；读取器拒绝版本或实例不匹配、无效公钥、不安全权限及链接文件。缺失清单不授权。

## 公钥池

条目按规范化公钥内容关联成员，ID 独立于成员 ID；多名成员可共用公钥。撤销最后一个引用成员时删除该公钥的全部条目，撤销只影响后续认证。接管保留池中全部公钥，补齐活跃成员缺少的公钥，也可手动「补齐用户公钥」。

`used` 表示当前有成员引用，`free` 表示没有；**free 公钥仍可用于 SSH 认证**。管理员可逐条清理 free 条目，服务器在事务和文件锁内复核引用，已被引用时返回 409。取消接管后可查看，不能写入或清理。

容器公钥写入所分配容器的 root `authorized_keys`，容器可使用正常 shell。容器设置中的 `ProxyJump` 仅生成客户端连接命令，例如 `alpha-jump@<地址>:<端口>`。

## 注册与补申请

注册事务提交邀请码扣减、成员、资源令牌摘要和资源任务，随后后台分配。`201` 仅表示登记成功；单项失败保留错误及已成功资源，不重复扣名额。

调用本人 API 携带 `Authorization: Bearer <resource_token>`，无需平台 Cookie 或 CSRF，仍校验 Host/Origin。令牌只能访问本人的资源，丢失后由管理员重置。总控只保存 SHA-256 摘要。

| 方法 | 用户 API | 请求 / 返回 |
| --- | --- | --- |
| GET | `/api/members/me/resources` | 本人 `access` 和全部当前 node；包括尚未申请的 node |
| POST | `/api/members/me/containers` | `{"node_id":"…"}`；探测在线后立即创建，成功返回 200 和已就绪资源，失败返回具体错误；支持新添加的 node |
| POST | `/api/members/me/retry` | `{}`；重试失败的跳板公钥写入和 node，返回 202 |
| GET | `/api/status/<使用者id>` | 本人状态页数据；全部 node 的基本信息、在线状态、容器总数及仅属于本人的容器 |
| POST | `/api/status/<使用者id>/containers` | `{"node_id":"…"}`；与本人容器申请接口共用创建流程，URL 使用者必须与令牌一致 |

`access` 返回分享节点、邀请链接及状态、公钥状态和错误；`nodes` 返回各 worker 的分配状态、容器 ID/名称、端口及 SSH 主机，不返回服务凭据或 root 密码。

总控 `/status/<id>` 页面凭本人令牌查看节点及容器，并申请尚未分配的在线节点；令牌仅存当前标签页的 `sessionStorage`，退出清除。离线节点保留中央分配记录，运行状态来自最近采集。

每个 `(member_id,node_id)` 只有一个分配槽；worker 保存创建计划，断线或重启后核对同一容器继续。重复申请返回现有分配，外部容器或未标记的数据目录冲突时拒绝。状态为 `unallocated/pending/running/ready/failed/deleting/deleted`；失败须显式重试，同一成员正在处理时返回 409，后台最多并发 8 个 worker。

## Tailscale 分享与核对

每名成员得到一个单次节点分享链接，用自己的 Tailscale 账号接受；管理员可刷新接受状态。API Key 使用 AES-GCM 加密，密钥为 control 数据目录的 `tailscale-api-token.key`（0600），缺失或损坏时报错。保存配置须带 revision；已有分享池时不能切换 Tailnet 或清除凭据，可替换到期 Key。

创建邀请结果明确被拒绝时可重试；断线、5xx、无效响应或持久化中断时保留 `unknown/creating`，避免重复分享。管理员核对节点邀请列表，关联本次新增的单次邀请 ID；不能关联旧邀请或其他成员已用邀请。确认没有新增邀请或已手动撤销后，可提交 `confirm_absent:true`，服务端再次核对列表后才允许重试。

## 回收与管理接口

删除成员先标记 `deleting`，停止本人令牌访问和新申请，再撤销分享、公钥，以及本流程分配的容器和带成员标记的 workspace/home。共享 `/data`、其他公钥和流程外容器保留。全部回收成功后删除成员及资源记录，保留审计和邀请码累计用量。

离线、失败或结果未知时保留记录，修复后再次 `DELETE` 继续；仍有资源引用的 worker 或分享节点不能移除。worker 保留已回收成员 ID 的分配槽，阻止迟到请求重建资源。

所有管理写操作要求平台管理员会话和 `X-CSRF-Token`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/bastion/resources` | 分享池、跳板安装状态及成员公钥状态；`jump_installation.web` 包含服务用户、网页 sudo 可用状态及原因；`account` 包含 `exists/managed/released/removed`，`data_directory` 为当前 control 目录；`key_pool` 包含条目及读取错误 |
| POST | `/api/bastion/install` | `{action:"init/adopt/release/delete",sudo_password:"本次 sudo 密码"}`；删除额外要求 `confirm:"alpha-jump"`；管理员会话及 CSRF 校验，同步操作，不接受客户端指定用户或目录；并发操作返回 409 |
| GET | `/api/bastion/keys` | 公钥池条目、指纹、关联用户及 `used/free` 状态 |
| POST | `/api/bastion/keys/sync` | `{}`；补齐活跃用户缺少的公钥，保留 free 条目 |
| DELETE | `/api/bastion/keys/<entryId>` | `{}`；仅清理当前无用户引用的 free 条目；有关联时返回 409 |
| GET / PUT | `/api/tailscale/settings` | `{revision,tailnet,api_token}` 配置；读取不返回 Key |
| POST | `/api/tailscale/test` | `{}` 测试已保存凭据 |
| GET | `/api/tailscale/devices` | 读取当前网络设备 |
| PUT | `/api/bastion/tailscale/<nodeId>` | `{enabled:true/false}` 加入分享池或启停 |
| GET | `/api/bastion/tailscale/<nodeId>/invites` | 核对节点现有邀请 |
| DELETE | `/api/bastion/tailscale/<nodeId>` | 无使用者引用时移除分享池配置 |
| POST | `/api/bastion/members/<id>/refresh` | `{}` 同步已知邀请接受状态 |
| POST | `/api/bastion/members/<id>/resolve-invite` | `{invite_id:"…"}` 关联结果未知的邀请，或 `{confirm_absent:true}` 确认无残留 |
| GET | `/api/members/<id>/resources` | 管理员查询资源 |
| POST | `/api/members/<id>/retry` | `{}` 重试资源分配 |
| POST | `/api/members/<id>/containers` | `{node_id:"…"}` 立即申请指定 node，成功返回 200，失败返回错误 |
| POST | `/api/members/<id>/token` | `{}` 重置并返回一次新的 `resource_token` |
| DELETE | `/api/members/<id>` | `{}` 排队回收并删除使用者；返回 202 |

worker 内部接口为 `PUT/DELETE /api/containers/members/<member_id>`，由总控后台使用服务凭据调用，不经浏览器节点代理开放。
