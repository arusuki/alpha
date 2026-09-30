# 集群使用者登记

集群使用者是机器/容器的使用者，独立于 Alpha 运维平台的登录账号。管理员在「集群使用者」页面配置注册字段、生成带名额限制的邀请码并查看登记信息。注册不会创建平台账号、密码或登录会话，也不会创建宿主机系统账号。

使用者、注册 schema 和邀请码由总控统一维护；registry 只为注册会话缓存表单定义与提交内容，邀请码仅存摘要。`id` 是稳定主键，`username` 是唯一且不可修改的使用者标识，用来关联各 node 的容器归属。总控创建容器或设置非空归属时，要求选择已登记的 `username`。CLI 导入或 Docker 标签中尚未登记的归属会在“使用者容器”中单列为“未登记使用者”；同名使用者登记成功后自动归入其统计，node 原始数据保持不变。

“使用者容器”展示使用者在各 node 上的容器及数量，同一个容器 ID 出现在不同 node 时分别计数。统计规则和离线行为见 [集群管理](cluster.md)。注册后自动分配分享节点、添加 alpha-jump 公钥，并在各 node 创建一个公钥登录容器；失败项可由使用者后续通过 API 补申请。详见 [跳板机与使用者资源](bastion.md)。

公网用户可通过 [registry](operations.md#公网-registry) 注册同一类集群使用者：访问 `/registry/<8位 REG_PASS>/<邀请码>`，经 control 校验后填写表单并查看资源分配进度和 Tailscale 分享链接。registry 不创建平台管理员或只读账号；control 不需要对公网开放入站端口。

数据格式与旧数据处理见 [运行与配置](operations.md#配置与数据)。

## 管理接口

以下接口要求平台管理员登录，写操作还需要 `X-CSRF-Token`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| PUT | `/api/members/registration-schema` | 保存 schema，带当前 `revision` 和完整 `fields` 数组 |
| GET | `/api/members` | 列出使用者、登记信息、注册时的 schema、来源邀请码 ID 和注册时间 |

## 页面中的邀请码管理

邀请码管理只通过管理员页面提供，不提供 JSON 管理 API。页面提交表单，服务器校验管理员会话和 CSRF 后渲染邀请码列表或本次生成结果。

在「注册邀请码」中填写备注和可注册人数（quota），点击生成后复制给使用者。quota 必须是 1–100000 的整数，备注最多 100 个字符。邀请码是 48 位密码学随机十六进制字符串，仅在生成时展示一次；数据库只保存 SHA-256 摘要。丢失后可在页面作废并生成新的邀请码。

页面显示已用、总名额、剩余和状态（可使用、名额已用尽、已作废）。已用是累计成功注册人数，剩余是当前可用名额，作废后为 0。邀请码不按时间过期，用尽或手动作废后失效；作废不影响已经注册的使用者。每次创建、作废、schema 修改和成功注册都写入操作审计；审计不含邀请码明文、密码或用户 profile。

## 注册 schema

`GET /api/members/registration-schema` 无需登录，供外部注册页面取得表单定义。初始值为 `{"revision":1,"fields":[]}`，表示只要求固定的使用者标识、邀请码和 SSH 公钥，没有附加信息字段。保存示例：

```json
{
  "revision": 1,
  "fields": [
    {"key":"full_name","label":"姓名","type":"text","required":true},
    {"key":"degree","label":"学历","type":"select","required":true,"options":["博士","硕士"]},
    {"key":"group","label":"组别","type":"select","required":true,"options":["A组","B组"]}
  ]
}
```

成功保存返回完整 schema，`revision` 加一。保存时版本不符返回 `409`，管理员应重新读取再编辑。

- 最多 32 个字段。`key` 唯一，为小写字母开头的 1–48 位小写字母、数字或下划线，`constructor` / `prototype` 保留；`label` 去掉首尾空白后为 1–80 个字符。
- `type` 支持 `text` 和 `select`。`required` 默认为 `false`。
- 单选字段需要 1–64 个非空、不重复的选项，每项最多 80 个字符，无换行。文本字段不接受非空选项。
- 注册 `profile` 的所有值必须是字符串。去掉首尾空白后，文本最多 512 个字符；单选值必须属于选项。可选字段可省略或提交空字符串，空字符串不保存；`null`、数组、未知字段均拒绝。
- 修改 schema 只影响后续注册。每名使用者保存注册时的 schema 快照，删除字段或改变名称、选项不会重新解释已有信息。

## 注册 API

`POST /api/members/register` 无需平台登录或 CSRF token，要求有效邀请码，不接受 `password`、`role` 等平台账号字段。

```bash
curl -X POST http://127.0.0.1:8765/api/members/register \
  -H 'Content-Type: application/json' \
  -d '{
    "username": "alice",
    "invitation_code": "替换为管理员发放的邀请码",
    "ssh_public_key": "ssh-ed25519 AAAA…",
    "schema_revision": 2,
    "profile": {"full_name":"张三","degree":"博士","group":"A组"}
  }'
```

`username` 必须以小写字母开头，为 3–32 位小写字母、数字、下划线或短横线，不能是 `data`。`schema_revision` 使用刚读取的版本，`profile` 必须是对象，没有附加字段时传 `{}`。

`ssh_public_key` 必填，接受单行 Ed25519、RSA（至少 2048 位）或 ECDSA 公钥，不接受 authorized_keys 选项、多行、私钥或证书。公钥发布到固定 `alpha-jump` 的专用授权清单，并写入各 node 容器。跳板需先完成一次 sudo 初始化，后续公钥管理免 sudo。

成功返回 `201`（资源后台创建）：

```json
{
  "id": "a7e1b46c28d149d0835f621b091a65c0",
  "username": "alice",
  "profile": {"full_name":"张三","degree":"博士","group":"A组"},
  "schema_revision": 2,
  "created_at": 1790730000,
  "resource_status": "pending",
  "resource_token": "一次性返回的本人资源令牌"
}
```

邀请码扣减、登记记录、资源分配记录、任务和审计在一个 SQLite 写事务中提交，并发注册不能超过 quota。重复用户名、字段错误、schema 版本冲突及数据库写入失败都会回滚，名额不变。注册成功后该使用者仍不能登录 Alpha 平台；请保存返回的本人资源令牌，使用 `/api/members/me/resources` 查询资源、`POST /api/members/me/containers` 补申请在线 node。详见 [资源 API 与回收](bastion.md)。请求超时且结果未知时，可由管理员在列表中核实是否登记成功；重试同一用户名不会再次消耗名额。

使用者网页入口为总控 `/status/<id>`，其中 `id` 是注册响应的使用者 ID。输入注册返回的 `resource_token` 后可查看全部 node 及自己的容器，在未分配的在线 node 点击加号立即申请。申请返回创建结果，失败显示具体错误；不需要运维平台登录。

| 状态码 | 含义 |
| --- | --- |
| 400 | 字段、类型、选项无效，或邀请码不存在、用尽、作废 |
| 401 / 403 | 管理接口未登录、权限不足、CSRF 或 Host/Origin 校验失败 |
| 409 | 使用者标识已存在，或 schema 版本已变化，需重新获取表单 |
| 413 / 415 | 请求超过 65536 字节，或未使用 `application/json` |
| 429 | 同一来源 IP 在 5 分钟内超过 20 次注册尝试；成功和失败均计数 |

公开接口继承平台 Host/Origin 检查，不开放跨域浏览器注册。外部注册服务可由服务端调用 API，或通过同源反向代理接入；所有 schema 字段名称和选项对注册客户端可见。限流按直接连接 IP 计算，不信任客户端提供的转发头；经代理调用时共享代理的额度。邀请码管理和使用者列表始终仅向管理员开放。
