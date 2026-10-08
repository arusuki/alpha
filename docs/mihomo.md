# Mihomo 代理管理

管理员在总控侧栏打开 **代理管理**。这一页集中管理总控本机、registry 和 worker 的订阅、过滤规则、配置模板、服务状态及策略组选择；计算节点工作台和公网注册页不另设代理配置入口。

## 准备核心与启动

在每台需要运行代理的主机上安装适合其 CPU 架构的 mihomo 可执行文件，并让 project-alpha 的服务账号拥有执行权限。在 **核心路径** 填写绝对路径，或填写 `mihomo` 使用该服务的 `PATH`。路径按目标机器解释，不能填写 shell 命令或附加参数。

1. 在 **总控默认配置** 添加订阅，设置过滤规则与模板。
2. 点击 **预览生成配置** 核对节点数量、策略组和生成的 YAML。
3. 点击 **保存并下发配置**。总控在后台抓取订阅、生成每个节点的配置，目标节点通过 `mihomo -t` 校验后应用。
4. 在节点列表点击 **管理**，确认配置已应用，再点击 **启动代理**。
5. 在同页的策略组中选择出口。仅 `select` 组提供手动切换；其他组显示核心的实际选择与最终出口。

核心由对应角色的 project-alpha 进程直接托管，以同一个普通用户运行，不需要单独安装 systemd 单元或使用 root。停止/重启平台会停止所托管的核心；平台重启时恢复各节点保存的启停状态和选择。核心意外退出会显示错误，可以在面板重启。首次保存配置不会自动启动代理。

生成配置默认只监听回环地址的 mixed HTTP/SOCKS 端口。模板可配置 DNS、规则与其他 mihomo 功能，但低端口、TUN 和透明代理仍受目标服务账号的系统权限约束。更换正在运行的核心路径前，先停止该节点代理。

## 继承与单独设置

所有新节点默认继承总控的订阅、刷新间隔、过滤规则、模板、核心路径和模板端口变量。总控保存后会更新继承节点；离线或应用失败的节点保留上次配置，并在后台重试未完成的下发。

选择任一 worker 或 registry，取消 **继承总控配置**，修改并保存即可为这个节点单独设置。重新勾选继承并保存，会恢复总控配置。总控的后续更改不会覆盖独立配置。

启停状态和策略组选择属于每个节点的运行状态，独立保存，不会因为继承配置而统一启停。更新后仍存在的候选选择会保留；已移除的组或候选不再恢复，核心使用新组的默认选择。关闭状态下也可以保存选择，供下次启动使用。

## 订阅与过滤

每个订阅设置唯一名称，填写 HTTP(S) URL 或内联内容其中之一。支持含内联 `proxies` 列表的 Clash/Mihomo YAML，保留协议的其他字段；只读取代理节点，不沿用订阅里的策略组、DNS、规则或外部控制器。上述配置由自己的模板管理。

也支持 URI 列表及其 base64 编码：SS、VMess、VLESS、Trojan、Hysteria2/Hy2。复杂传输或未支持的 URI 参数应使用原生 YAML，错误会指出不支持的字段。订阅 URL 使用 `clash.meta/project-alpha` User-Agent，单次读取最多 8 MiB。多个订阅合计最多 10,000 个节点；重名时需要设置订阅名称前缀，不静默覆盖。

命名过滤规则包含：

| 字段 | 含义 |
| --- | --- |
| 名称 | 模板通过 `names "规则名称"` 引用，`all` 为保留名称 |
| 名称包含 | RE2 正则，留空表示不限制 |
| 名称排除 | RE2 正则，匹配时排除，留空表示不排除 |
| 协议类型 | 逗号分隔，如 `ss, vmess, vless`；留空不限 |
| 订阅来源名称 | 逗号分隔；留空不限 |

例如规则 `美国`：包含 `美国|US`，排除 `0\.0?1`。正则默认区分大小写，可使用 `(?i)`。同一条规则的条件同时生效。**全局过滤规则** 先过滤合并的订阅；模板引用的命名规则在这个结果上继续筛选。`all` 或留空表示保留全部。

默认每 720 分钟更新一次订阅，可设为 5–10080 分钟，或设为 0 仅手动刷新。页面可刷新单节点或全部订阅。一个来源失败时，不生成缺少该来源的部分配置；旧配置继续可用。

## 模板

模板使用 Go `text/template`，支持 `if`、`range`、变量、比较与管道，不提供文件读取、环境变量或命令执行函数。生成结果须为单份 YAML，包含 `proxies` 和非空 `proxy-groups`。

| 变量 / 函数 | 用途 |
| --- | --- |
| `.Proxies` | 经全局过滤的完整代理对象列表 |
| `.Names` | 上述代理名称列表 |
| `.MixedPort` | 面板设置的代理端口 |
| `.Node.ID` / `.Node.Name` / `.Node.Role` | 当前节点 ID、名称与角色；本机 ID 为 `control` |
| `names "规则名称"` | 取得指定规则筛选后的名称列表；`all` 表示全部 |
| `yaml 值` | YAML 编码，适用于对象或列表 |
| `quote 值` | 编码标量，避免名称中的引号或冒号破坏 YAML |
| `indent 数字 字符串` | 每行添加指定空格，支持 0–32 |

示例：先在界面创建 `美国` 规则，再使用：

```yaml
mixed-port: {{ .MixedPort }}
allow-lan: false
mode: rule
log-level: warning
proxies:
{{ yaml .Proxies | indent 2 }}
proxy-groups:
  - name: 美国自动
    type: url-test
    proxies:
{{ names "美国" | yaml | indent 6 }}
    url: https://www.gstatic.com/generate_204
    interval: 300
    lazy: true
  - name: 出口
    type: select
    proxies:
      - 美国自动
      - DIRECT
{{ range (names "美国") }}      - {{ quote . }}
{{ end }}rules:
  - MATCH,出口
```

该示例中的测速由 mihomo 策略组执行，project-alpha 不添加独立的定时健康检查。

同一个模板可以按角色分支，例如同机运行多个角色时避免端口冲突：

```yaml
mixed-port: {{ if eq .Node.Role "registry" }}7891{{ else if eq .Node.Role "worker" }}7892{{ else }}{{ .MixedPort }}{{ end }}
```

未知规则名、无效正则、空策略组、重复名称、不存在的候选和循环引用会明确报错。策略组使用明确的 `proxies` 候选列表，不支持模板里的 `proxy-providers`、`use` 或 `include-all*`；代理来源统一从订阅区管理。`rule-providers` 等其他核心配置可以写进模板，并接受目标 mihomo 的最终校验。

## 状态、持久化与恢复

节点表分别显示平台节点是否可连接、核心是否运行，以及配置是否成功下发。后台订阅抓取或下发失败会展示错误；网络恢复或修复核心路径后会重试。若想立即重试，可点击 **刷新此节点订阅**。正在运行的服务使用核心 [配置重载与代理选择 API](https://wiki.metacubex.one/api/)。

订阅 URL、内联凭据、模板和生成配置在 SQLite 中加密保存，密钥为数据目录中的 `mihomo.key`（0600）。备份时须与数据库一起保留；密钥缺失或损坏会报错，不会自动生成替代密钥覆盖已有内容。管理员配置接口可以回读这些内容以供编辑，仅管理员可访问代理管理 API。

当前生成文件位于 `数据目录/mihomo/config.yaml`（0600）。该目录为 0700，核心控制器使用目录内的 Unix socket，不开放控制器 TCP 端口。模板中的 `external-*` 和 `secret` 由服务接管；不会沿用订阅或模板的公开控制地址。[mihomo 的 Unix 控制接口本身不校验 secret](https://wiki.metacubex.one/en/config/general/)，因此使用目录权限限制访问。

数据库迁移由服务初始化与独立 updater 共用。目标版本见 `alpha-updater --help`，支持窗口见生成的 `internal/platform/upgrade_history.json`。离线原地升级方式见 [updater 文档](updater.md#原地升级与故障恢复)，无需更换开发数据目录。

## 验证

```bash
go test -race ./internal/mihomo ./cmd/alpha-updater
python3 tests/test_mihomo_browser.py
# 使用已安装的真实核心，所有流量与数据仍限定在本机临时测试环境。
PROJECT_ALPHA_TEST_MIHOMO=/absolute/path/to/mihomo python3 tests/test_mihomo_integration.py
```

真实核心测试覆盖三个角色的配置继承、实际代理请求、策略选择、错误配置保留、恢复继承、父进程退出、重启恢复和管理员权限边界。不读取真实订阅，也不下载核心或测试密钥。
