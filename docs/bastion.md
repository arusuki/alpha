# 跳板机与使用者资源

总控「跳板机管理」维护 Tailscale 分享节点池及固定 `alpha-jump` 账号，提供添加、接管、删除和取消接管选项。跳板初始化或接管后，注册时添加成员公钥，删除使用者时撤销公钥。Tailscale 分享节点按关联人数最少优先选择，同数按 ID 排序；停用分享节点只影响后续分配。

数据库格式 **v23**，worker 节点协议 **v3**。总控、worker 和 registry 使用独立的新数据目录，不迁移、不删除或覆盖旧目录。

## 配置

1. 保存个人 Tailscale API Key（`tskey-api-…`）和 Tailnet（`-` 为当前凭据所属网络）。点击查询节点，从已授权的自有节点中选择允许分享的节点。总控无需安装 Tailscale 客户端。
2. 没有账号时点击「添加账号」；已有 `alpha-jump` 时点击「接管已有账号」。输入当前服务用户的 sudo 密码，安装或更新公钥读取器并保留有效的已有 data。此后总控以普通用户添加、读取和撤销成员公钥，无需配置跳板地址、端口或账号池。
3. 在每个 node 的容器管理中配置默认镜像、数据目录、Docker endpoint、起始端口和 SSH 访问地址。注册容器沿用默认镜像，使用现有 bridge 网络、全部 GPU 和自动端口分配。
4. 在集群使用者页面生成注册邀请码，通过 API 或 [公网 registry](../README.md#公网-registry) 提交使用者信息和 SSH 公钥。registry 展示实时进度及分享链接；直接调用 API 注册的使用者可打开总控 `/status/<使用者id>`，凭本人资源令牌查看节点和申请容器；管理员的使用者列表提供页面链接。

## 一次性初始化与权限

先安装并配置 OpenSSH 服务，以现有普通系统用户运行 control。管理员在「跳板机管理」选择「添加账号」或「接管已有账号」，输入页面显示的服务用户的 sudo 密码；这是宿主机系统密码，不是平台登录密码。安装用户和数据目录由服务端确定，网页不能指定其他用户或目录。

服务用户需具备执行当前二进制 `bastion install-helper` 的 sudo 权限。使用 systemd 时，将 `deploy/project-alpha.service` 的 `User=` 设置为该已有用户，不使用 `DynamicUser`；网页安装要求 `NoNewPrivileges=false`，模板已按此配置。已有部署如仍使用 `true`，须修改单元、执行 `systemctl daemon-reload` 并重启服务；页面会显示限制原因。`NoNewPrivileges` 对 sudo 的限制见 [systemd 官方说明](https://github.com/systemd/systemd/blob/main/man/systemd.exec.xml)。完成安装且不再需要网页重装或其他 sudo 操作时，可恢复为 `true`，日常公钥管理不受影响。

每次账号操作只启动一次 `/usr/bin/sudo -S -k`，忽略且不更新 sudo 凭据缓存。密码仅在当前请求内存中短暂持有，识别到 sudo 提示后经匿名 stdin 管道发送一次，随后清空可变字节缓冲区；安装辅助程序就绪后才发送用户及目录，它不会收到密码。密码不进入命令行、环境变量、数据库、审计、临时文件或浏览器存储。sudo/PAM 原始 stderr 被丢弃，认证失败显示固定错误；下次提交必须重新输入。弹窗在提交、关闭、离开页面及退出登录时清空密码，并禁用自动填充。认证最多等待 30 秒，整次账号操作请求最多 45 秒；关闭弹窗会取消请求并关闭控制管道，辅助程序检测 EOF 后停止。中断后应刷新安装状态，必要时重新安装。

也可在终端初始化（下面以服务用户 `yuuka` 为例）：

```sh
sudo ./bin/project-alpha bastion init --service-user yuuka --data-dir /var/lib/project-alpha-control
```

初始化会以指定服务用户创建或核验 control 数据目录，因此数据库不会归 root 所有。目录上级必须存在；已有目录须属于该服务用户且不允许组或其他用户写入。数据库格式不符、角色不符时明确拒绝，使用新目录，不自动迁移或覆盖。

随后以该普通用户启动服务：

```sh
./bin/project-alpha --control --data-dir /var/lib/project-alpha-control
```

终端初始化时，`--service-user` 须与日后运行 control 的用户一致。日常主进程不需要提权、不加入跳板用户组、不读写跳板 home。

初始化执行以下安装：

- 创建专用系统用户及组 `alpha-jump`，home 为 root 持有的 `/var/empty/alpha-jump`，shell 为 `/bin/false`，密码设为不可用于密码登录的值。已有同名账号必须显式选择接管；接管保留其 UID/GID、home 和系统账号属性，安装受限 SSH 配置。home 中的原 `authorized_keys` 保留，但此后 SSH 只接受平台公钥清单。
- 在 `/var/lib/project-alpha-jump/installation.json` 保存 root 持有的安装记录，绑定 control 实例 ID、服务用户名/UID 和跳板 UID/GID；同目录下的 `released`、`account-removed` 标记单独记录取消接管和账号删除状态，不改变原有安装记录或公钥清单格式；普通写入必须匹配当前绑定；显式接管允许转移 control 和服务用户绑定，跳板 UID/GID 保持不变。
- 安装 root 持有的 `/usr/local/libexec/project-alpha-jump`，供 sshd 执行 `bastion authorized-keys %u %U`。它只读取专用公钥清单，不打开 control 数据库。
- 创建 `/var/lib/project-alpha-jump/keys`，属主为服务用户、组为 `alpha-jump`，权限 `2750`。服务用户可发布公钥，跳板用户只能读取；公钥清单为 `0640`，root 持有的上级目录禁止替换安装记录。
- 在 `/etc/ssh/sshd_config` 末尾写入带标记的 `Match User alpha-jump` 配置。首次改动前保留 `.before-alpha-jump` 备份；先用 `sshd -t` 校验候选配置，再用 `sshd -T -C` 检查该账号的有效配置，发现已有规则覆盖时拒绝生效。验证成功后重载活动的 `ssh.service` 或 `sshd.service`；重载失败恢复修改前的配置。

专用账号使用 `AuthorizedKeysFile none`，只接受公钥读取器返回的授权；`AuthorizedKeysCommandUser alpha-jump` 保证读取器也是非 root。SSH 策略禁止密码认证、shell/SFTP、PTY、Agent、X11 和 Unix socket 转发，只允许 ProxyJump 所需的客户端本地 TCP 转发。已有其他 SSH 用户的配置不变。OpenSSH 的命令和转发选项见 [官方文档](https://man.openbsd.org/sshd_config.5)。

同一实例重复初始化会保留公钥并更新读取器；不会清空已有清单。显式接管允许使用当前 control 数据目录和当前服务用户，不要求原绑定一致，原服务用户已不存在也可接管。先校验现有公钥池及跳板 UID/GID，准备包含全部公钥的新目录，再原子交换安装目录，转移写入权限；原安装目录完整保留为 `/var/lib/project-alpha-jump.before-adopt-*`，旧 control 数据库保留。普通初始化、发布、取消接管和删除不会隐式转移归属。缺少安装记录但已有公钥 data 时仍明确报错。格式不匹配须使用新数据目录，不做迁移。网页安装始终校验并重载 SSH；自行管理 sshd（例如容器内）时可在终端使用 `--no-reload`，此时只安装和校验，须另行重载或启动 sshd 后才生效。

网页根据账号状态提供这些选项，全部沿用本次 sudo 密码验证及审计：

| 选项 | 行为 |
| --- | --- |
| 添加账号 / 重新安装跳板 | 不存在时创建固定账号；当前已接管时更新工具与 SSH 配置，保留公钥 data |
| 接管已有账号 | 将现有账号、工具和公钥池转交当前 control / 服务用户，保留所有公钥并补齐当前用户的公钥 |
| 取消接管 | 仅停止平台添加和撤销公钥，保留账号、工具、data、home、SSH 配置和现有授权；不会撤销访问权限 |
| 删除账号 | 输入 `alpha-jump` 确认，先检查已发布公钥为空；只执行非强制账号删除，保留工具、data、home 和 SSH 配置 |

取消接管后，成员新增公钥或撤销会报错并保留待处理记录；重新接管后可重试。已经发布的公钥仍可查询和认证。删除账号前需先回收使用者的跳板公钥；账号仍有进程时删除会失败，不主动终止连接。删除后可重新添加，复用记录中的 UID/GID；身份冲突时保留 data 并报错。所有账号操作与公钥发布共用文件锁，避免取消接管后仍有在途写入，或删除检查后又发布新公钥。

对应终端命令（接管时填写当前 control 的服务用户和数据目录）：

```sh
sudo ./bin/project-alpha bastion adopt --service-user yuuka --data-dir /var/lib/project-alpha-control
sudo ./bin/project-alpha bastion release --service-user yuuka --data-dir /var/lib/project-alpha-control
sudo ./bin/project-alpha bastion delete --service-user yuuka --data-dir /var/lib/project-alpha-control --confirm alpha-jump
```

`release` 和 `delete` 不修改或重载 SSH 配置。`init` 和 `adopt` 在自行管理 sshd 时可加 `--no-reload`。

后续注册、重试和删除均由普通 control 进程原子更新 `keys/keys.json`，使用文件锁、文件和目录同步；发布成功后才标记授权就绪或已撤销。读取器校验完整清单，拒绝版本/实例不匹配、非法公钥、符号链接、硬链接及不安全权限；每行由读取器附加 `restrict,port-forwarding,command="/bin/false"` 和公钥池条目标识。用户与条目按规范化公钥内容关联，条目 ID 不要求等于用户 ID；多名用户可以共用一条公钥。删除用户时，其他用户仍引用的公钥保留；最后一名引用用户回收时，撤销该公钥的全部池条目。缺失清单返回零条公钥，损坏清单拒绝授权；不回退到用户 home。

「跳板公钥池」展示每个条目的公钥、指纹、关联用户和 `used` / `free` 状态。池可以是当前用户公钥的超集：接管时已有公钥全部保留，缺失的当前活跃用户公钥自动补入，已存在的公钥直接关联，不重复添加。也可点击「补齐用户公钥」重试。`free` 仅表示未被当前用户引用，仍能用于 SSH 认证；管理员可逐条清理。清理在数据库事务及公钥文件锁内重新检查用户关联，刚被用户引用的条目返回 409，不会按过期页面状态删除。取消接管后可查看池，写入和清理停止。

清单独立于 control 进程生命周期，短暂重启不影响已发布公钥的查询；撤销只影响后续 SSH 认证，不主动中断已有 SSH 连接。容器公钥仍写入各 worker 所分配容器的 root `authorized_keys`，允许正常容器 shell。容器设置中的 `ProxyJump` 仅生成客户端连接命令，可填写 `alpha-jump@<跳板地址>:<端口>`，不会修改任何系统账号。

## 注册与补申请

注册事务一起提交邀请码扣减、使用者、公钥、本人资源令牌摘要、跳板公钥状态和各现有 node 的容器分配记录。随后后台执行资源申请，返回 `201` 表示登记成功，**不表示全部资源已就绪**。未配置 Tailscale 分享池时保留待分配记录；公钥写入失败时保留错误，修复后可重试。单个 node 失败不撤销其他 node 成功的容器，也不再次扣邀请码名额。

注册返回一次性的 `resource_token`，数据库只保存 SHA-256 摘要。它只能查询和重试本人的资源，不能登录运维平台、指定他人、修改镜像/端口/GPU、管理 node 或删除使用者。不将令牌放入 URL；调用携带 `Authorization: Bearer <resource_token>`，不需要平台 Cookie 或 CSRF。仍继承 Host/Origin 限制。令牌丢失由管理员重置，旧令牌立即失效。

| 方法 | 用户 API | 请求 / 返回 |
| --- | --- | --- |
| GET | `/api/members/me/resources` | 本人 `access` 和全部当前 node；包括尚未申请的 node |
| POST | `/api/members/me/containers` | `{"node_id":"…"}`；探测在线后立即创建，成功返回 200 和已就绪资源，失败返回具体错误；支持新添加的 node |
| POST | `/api/members/me/retry` | `{}`；重试失败的跳板公钥写入和 node，返回 202 |
| GET | `/api/status/<使用者id>` | 本人状态页数据；全部 node 的基本信息、在线状态、容器总数及仅属于本人的容器 |
| POST | `/api/status/<使用者id>/containers` | `{"node_id":"…"}`；与本人容器申请接口共用创建流程，URL 使用者必须与令牌一致 |

资源查询包含 `member_id`、`status`、`access`、`nodes`。`access` 包含分享节点、邀请 ID/链接/状态、已接受的 Tailscale 账号、公钥状态和错误。跳板公钥统一属于 `alpha-jump`。`nodes` 包含 node ID/名称、容器 ID/名称、端口、SSH 主机、状态和错误。不会返回 node 令牌、Tailscale API Key、其他使用者资源或容器随机 root 密码。

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
