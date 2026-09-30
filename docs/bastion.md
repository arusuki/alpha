# 跳板机与使用者资源

总控「跳板机管理」维护两个共享资源池：可分享的 Tailscale 节点，以及总控主机上已有的非 root 跳板账号。资源可分配给多名使用者；按当前关联人数最少优先选择，同数按 ID 排序。停用只影响后续分配。注册、分享、跳板授权和 node 容器都有独立记录。

数据库格式 **v19**，节点协议 **v3**。总控与 worker 使用新数据目录，不迁移、不删除或覆盖旧目录。

## 配置

1. 保存个人 Tailscale API Key（`tskey-api-…`）和 Tailnet（`-` 为当前凭据所属网络）。点击查询节点，从已授权的自有节点中选择允许分享的节点。总控无需安装 Tailscale 客户端。
2. 添加总控主机上的现有非 root 用户，填写使用者可连接的 SSH 地址和端口。总控进程必须有读写对应 home 下 `.ssh/authorized_keys` 的权限；home 必须属于该用户、不能经过符号链接，且不能允许组或其他用户写入。不会创建或删除系统账号，也不修改 `sshd_config`；SSH 服务必须允许该账号使用公钥和 TCP 转发。
3. 在每个 node 的容器管理中配置默认镜像、数据目录、Docker endpoint、起始端口和 SSH 访问地址。注册容器沿用默认镜像，使用现有 bridge 网络、全部 GPU 和自动端口分配。
4. 在集群使用者页面生成注册邀请码，通过 API 提交使用者信息和 SSH 公钥。注册后打开总控 `/status/<使用者id>`，凭本人资源令牌查看节点和申请容器；管理员的使用者列表提供页面链接。

本机跳板账号记录用户名、UID、GID、home；身份改变时拒绝写入。每个公钥块含 `# project-alpha:member:<id>` 注释，公钥行也带同样注释。重复写入只保留一个成员块，回收只删除该成员块，保留其他手工公钥及其他成员（即使公钥相同）。采用文件锁和原子替换，拒绝符号链接公钥文件，`.ssh` 为 0700，`authorized_keys` 为 0600。跳板公钥附加 `restrict,port-forwarding,command="/bin/false"`，允许 TCP 转发，禁止 shell/PTY、Agent 和 X11 转发。容器内公钥写到 root 的 `authorized_keys`，保留无关公钥，允许正常容器 shell。

## 注册与补申请

注册事务一起提交邀请码扣减、使用者、公钥、本人资源令牌摘要、跳板分配和各现有 node 的容器分配记录。随后后台执行资源申请，返回 `201` 表示登记成功，**不表示全部资源已就绪**。没有配置资源池时保留待分配记录；后续配置好资源后可重试。单个 node 失败不撤销其他 node 成功的容器，也不再次扣邀请码名额。

注册返回一次性的 `resource_token`，数据库只保存 SHA-256 摘要。它只能查询和重试本人的资源，不能登录运维平台、指定他人、修改镜像/端口/GPU、管理 node 或删除使用者。不将令牌放入 URL；调用携带 `Authorization: Bearer <resource_token>`，不需要平台 Cookie 或 CSRF。仍继承 Host/Origin 限制。令牌丢失由管理员重置，旧令牌立即失效。

| 方法 | 用户 API | 请求 / 返回 |
| --- | --- | --- |
| GET | `/api/members/me/resources` | 本人 `access` 和全部当前 node；包括尚未申请的 node |
| POST | `/api/members/me/containers` | `{"node_id":"…"}`；探测在线后立即创建，成功返回 200 和已就绪资源，失败返回具体错误；支持新添加的 node |
| POST | `/api/members/me/retry` | `{}`；重试待分配跳板授权和失败的 node，返回 202 |
| GET | `/api/status/<使用者id>` | 本人状态页数据；全部 node 的基本信息、在线状态、容器总数及仅属于本人的容器 |
| POST | `/api/status/<使用者id>/containers` | `{"node_id":"…"}`；与本人容器申请接口共用创建流程，URL 使用者必须与令牌一致 |

资源查询包含 `member_id`、`status`、`access`、`nodes`。`access` 包含分享节点、跳板账号、SSH 地址/端口、邀请 ID/链接/状态、已接受的 Tailscale 账号、公钥状态和错误。`nodes` 包含 node ID/名称、容器 ID/名称、端口、SSH 主机、状态和错误。不会返回 node 令牌、Tailscale API Key、其他使用者资源或容器随机 root 密码。

状态页沿用独立的使用者身份，平台管理员 Cookie 不能代替本人资源令牌。页面将令牌保存在当前标签页的 `sessionStorage`，退出查看时清除，不放入地址或日志。页面每十秒刷新，显示节点名称、地址、主机、在线状态、容器总数和采集状态；容器名称同时包含注册流程的分配记录和节点清单中归属本人的已有容器。离线节点保留中央分配记录，运行状态来自最近采集，不表示实时 Docker 状态。尚无容器的在线节点提供「＋ 申请」，失败后显示错误并可重试；离线节点不排队新申请。创建期间防止重复提交，失败不会由后台不断重试。

```bash
curl -H 'Authorization: Bearer 替换为本人资源令牌' \
  http://127.0.0.1:8765/api/members/me/resources
curl -X POST -H 'Authorization: Bearer 替换为本人资源令牌' \
  -H 'Content-Type: application/json' \
  -d '{"node_id":"替换为资源查询返回的节点ID"}' \
  http://127.0.0.1:8765/api/members/me/containers
```

每个 `(member_id,node_id)` 只有一个中央记录；worker 同样以成员 ID 建立持久化分配槽，容器名固定为 `alpha-<member_id>`，同时带成员和 owner 标签。重复申请已经就绪的容器返回现有分配。节点端保存原 Docker daemon、创建计划、端口、启动门禁和数据路径；创建失败、启动失败、请求断线和服务重启后核对同一容器再继续，不创建第二份。发现同名但不属于此分配的容器、被更改的配置或未标记的既有数据目录时明确报错，保留现场供核对。创建 API 同样拒绝在该 node 已有管理容器的同一 owner 再创建容器。

节点状态：`unallocated` / `pending` / `running` / `ready` / `failed` / `deleting` / `deleted`。`failed` 可以重试，正在处理同一成员时返回 409，等待后重新查询；node 探测失败返回 502。后台不会不断重试已失败任务。重启恢复仍在执行的持久任务。应用资源每次最多并发 8 个 node。

## Tailscale 分享与核对

每名使用者得到一个**单次**节点分享链接，不通过邮件发送，不授予出口节点权限。接受链接需要使用者自己的 Tailscale 账号。领取链接的账号不由平台注册信息推断，管理员可「更新分享状态」查看实际 `acceptedBy`。

Tailscale 创建邀请 API 不支持幂等请求标识。发送前保存状态和邀请列表；正常返回即记录邀请 ID/链接。明确的 4xx 拒绝可以重试；断线、5xx、无效响应或创建后持久化中断保留 `unknown` / `creating` 状态，不盲目再次创建。管理员在使用者资源窗口查看节点邀请列表，核对本次新增邀请并关联其 ID；不能关联分配前已有的邀请、多次使用邀请或其他成员已经登记的邀请。无法确定邀请归属时，保留记录并在 Tailscale 控制台核对。管理员确认未生成邀请或已手动撤销后，可提交 `confirm_absent:true`；只有再次读取节点列表确认没有本次分配前列表之外的邀请时，才恢复为可重试状态。

API Key 在 SQLite 中 AES-GCM 加密，密钥文件为数据目录 `tailscale-api-token.key`（0600）；缺失或损坏明确报错，不自动重建。可留空保留 Key，可更换到期 Key。配置采用 revision 防止覆盖；存在分享池时禁止切换 Tailnet 或清除 Key，避免失去回收凭据。设置、节点和分享管理接口仅对平台管理员开放。错误不回显上游响应正文或密钥。

官方接口和单次邀请语义参考 [Tailscale API](https://tailscale.com/api) 及 [官方 OpenAPI](https://api.tailscale.com/api/v2)。

## 回收与管理接口

删除使用者会先标记 `deleting` 并立即停止本人令牌访问和新申请，再撤销登记的 Tailscale 邀请/分享、删除该成员的跳板公钥、停止并删除每个 node 的分配容器和带归属标记的专属 workspace/home 数据。共享 `/data`、其他使用者的公钥、系统跳板账号和 Tailscale 节点本身保留。

全部确认回收后才删除成员和资源记录，保留审计及邀请码累计已用名额。node 离线、权限失败、Tailscale 请求失败或结果未知时仍保留成员与失败记录；修复后再次 `DELETE` 继续回收。不能移除仍被使用者资源引用的 node 或分享池资源。worker 保留已回收成员 ID 的分配槽，防止迟到的旧创建请求重新生成已删除资源；新成员可以再次使用已释放的 username。

回收范围是本流程登记并创建的资源。扫描中发现的历史容器和管理员在此流程外创建的容器不凭 username 自动删除；发生已有 owner 容器冲突时需管理员核对其归属。

所有管理写操作要求平台管理员会话和 `X-CSRF-Token`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/bastion/resources` | 两类资源池及关联的使用者、状态、错误 |
| GET / PUT | `/api/tailscale/settings` | `{revision,tailnet,api_token}` 配置；读取不返回 Key |
| POST | `/api/tailscale/test` | `{}` 测试已保存凭据 |
| GET | `/api/tailscale/devices` | 读取当前网络设备 |
| PUT | `/api/bastion/tailscale/<nodeId>` | `{enabled:true/false}` 加入分享池或启停 |
| GET | `/api/bastion/tailscale/<nodeId>/invites` | 核对节点现有邀请 |
| POST | `/api/bastion/accounts` | `{username,host,port}` 添加现有本机账号 |
| PUT | `/api/bastion/accounts/<id>` | `{enabled:true/false}` 启停分配 |
| DELETE | `/api/bastion/tailscale/<nodeId>` 或 `/api/bastion/accounts/<id>` | 无使用者引用时移除配置 |
| POST | `/api/bastion/members/<id>/refresh` | `{}` 同步已知邀请接受状态 |
| POST | `/api/bastion/members/<id>/resolve-invite` | `{invite_id:"…"}` 关联结果未知的邀请，或 `{confirm_absent:true}` 确认无残留 |
| GET | `/api/members/<id>/resources` | 管理员查询资源 |
| POST | `/api/members/<id>/retry` | `{}` 重试资源分配 |
| POST | `/api/members/<id>/containers` | `{node_id:"…"}` 立即申请指定 node，成功返回 200，失败返回错误 |
| POST | `/api/members/<id>/token` | `{}` 重置并返回一次新的 `resource_token` |
| DELETE | `/api/members/<id>` | `{}` 排队回收并删除使用者；返回 202 |

worker 内部接口为 `PUT/DELETE /api/containers/members/<member_id>`，由总控后台使用服务凭据调用，不经浏览器节点代理开放。
