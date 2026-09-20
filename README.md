# cpa-plugin-codex-turn-state

CLIProxyAPI（CPA）原生插件，用于保存并复用上游返回的
`X-Codex-Turn-State`。插件按 **账号 + 实际模型** 独立管理票；业务请求到达时，有效票
直接注入，缺票则等待获取成功后再放行。

插件不会生成或伪造票，只保存上游实际返回的 292 字符票。

## 工作方式

推荐开启“按需打票”：

```text
业务请求到达
    │
    ├─ 当前账号 + 当前模型已有未过期的 292
    │      └─ 注入并放行，不请求上游打票
    │
    └─ 没有有效票
           └─ 同步打票
                ├─ 获得 292：保存、注入、放行业务
                └─ 次数或时间用尽：拒绝本次业务请求
```

账号空闲时不会主动打票，也不会定时续票。同一个账号和模型的并发请求共用一次获取；
同一账号下不同模型的票仍完全隔离。

## 292 和 312

- **292**：插件认可的有效票，可以保存和复用；
- **312**：降级状态，不保存为有效票，仍有可用出口和预算时继续尝试。

有效期从票内的 Fernet 时间戳起算。默认 TTL 为 3600 秒，保存时间不会延长票的寿命；
剩余不足 5 秒时视为不可用。

## 不可跨越的边界

1. 票不能跨账号复用；
2. 票不能跨模型复用；
3. 只保护显式选择的账号和模型，名单外请求保持 CPA 原有行为；
4. 按需模式只支持 HTTP/SSE；
5. 插件只改写 `X-Codex-Turn-State`，不会修改 CPA 内核、账号启停状态或 OAuth 凭证，
   也不会刷新 access token。

WebSocket 会被明确拒绝。CPA 会复用已经完成握手的连接，插件无法保证连接里的请求头能
随每条业务请求更新，因此当前实现不对 WebSocket 作错误承诺。

## 部署

插件需要安装到 **CPA 自己的 `plugins.dir`**，不是 CPAMP 的程序目录。CPAMP 可以用来
打开插件看板，但实际加载 `.so` 的进程是 CPA。

### 1. 构建动态库

Linux AMD64 可以在安装了 Docker 的构建机上执行：

```bash
git clone https://github.com/TSk615/cpa-plugin-codex-turn-state.git
cd cpa-plugin-codex-turn-state
bash scripts/build.sh
```

产物为：

```text
build/linux/amd64/codex-turn-state.so
```

脚本使用临时 Go 容器构建，不要求宿主安装 Go。动态库是 cgo `c-shared` 产物，因此
目标系统和 CPU 架构必须匹配。ARM64 服务器需要在其他构建机上以
`GOOS=linux GOARCH=arm64 CGO_ENABLED=1` 配合 ARM64 C 交叉编译器生成产物；低内存
生产服务器只安装成品，不建议现场构建。

### 2. 安装到 CPA 插件目录

先确认 CPA 配置或启动参数里的 `plugins.dir`。该目录通常按系统和架构分层：

```text
<plugins.dir>/linux/amd64/
<plugins.dir>/linux/arm64/
```

将产物放到匹配的目录：

```bash
PLUGIN_ROOT=/path/to/cpa-plugins
ARCH=amd64                       # ARM64 改为 arm64
INSTALL_DIR="$PLUGIN_ROOT/linux/$ARCH"

mkdir -p "$INSTALL_DIR"
install -m 0755 build/linux/$ARCH/codex-turn-state.so \
  "$INSTALL_DIR/.codex-turn-state.so.new"
mv -f "$INSTALL_DIR/.codex-turn-state.so.new" \
  "$INSTALL_DIR/codex-turn-state.so"
```

使用同一文件系统里的临时文件再 `mv`，可以避免直接覆盖正在加载的动态库。首次安装没有
旧文件；升级时建议先复制一份 `.so` 作为回滚版本。

如果 CPA 运行在 Docker 中，宿主上的 `PLUGIN_ROOT` 必须挂载到容器配置的
`plugins.dir`。`store_dir` 同样填写**容器内路径**，并挂载持久化目录，例如：

```yaml
services:
  cpa:
    volumes:
      - ./cpa-plugins:/CLIProxyAPI/plugins
      - ./cpa-data:/data
```

对应的插件目录和票存储目录可以设置为：

```text
plugins.dir = /CLIProxyAPI/plugins
store_dir   = /data/turn-state-store
```

### 3. 合并插件配置

把下一节的 `plugins.configs.codex-turn-state` 合并进 CPA 的 `config.yaml`。不要用示例
文件覆盖原配置，否则可能删除其他插件、账号或服务设置。

首次部署建议保持 `on_demand: false`，先启动 CPA 并确认插件成功加载，再从看板选择账号
和模型、设置次数与时间，最后开启按需打票。

### 4. 重启 CPA

CPA 只在启动时加载动态库。首次安装或替换 `.so` 后必须重启 CPA，例如：

```bash
docker restart <CPA容器名>
```

只保存看板中的按需设置通常不需要重启。

### 5. 验证

假设 CPA 监听 `127.0.0.1:8317`：

```bash
BASE=http://127.0.0.1:8317
RES="$BASE/v0/resource/plugins/codex-turn-state"

curl -f "$BASE/healthz"
curl -f "$RES/status"
curl -f "$RES/dashboard" | head
```

同时检查 CPA 日志，应看到插件被加载和注册；`status` 中应包含 `role`、`on_demand`、
`on_demand_max_attempts` 与 `on_demand_timeout_seconds`。如果看板或 `status` 返回 404，
通常是动态库没有放进 CPA 实际使用的 `plugins.dir`、架构目录不匹配，或 CPA 尚未重启。

### 6. 升级与回滚

升级前备份当前 `.so`，本地构建新产物后上传，按上面的临时文件加 `mv` 方式替换并重启
CPA。出现问题时恢复备份的 `.so` 再重启即可。票库和 `runtime.json` 位于 `store_dir`，
替换动态库不会主动删除它们。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    codex-turn-state:
      enabled: true
      priority: 100

      # 默认关闭，确认账号和模型后再开启。
      on_demand: false
      on_demand_accounts:
        - codex-REPLACE-WITH-EXACT-CREDENTIAL-FILENAME.json
      on_demand_models:
        - gpt-5.6-sol
        - gpt-6-astra

      # 任一条件先达到就停止本次获取。
      on_demand_max_attempts: 10
      on_demand_timeout_seconds: 90

      role: business
      dry_run: false
      inject_mode: replace-only

      template_length: 292
      replace_length: 312
      ttl_seconds: 3600
      store_dir: /data/turn-state-store

      # 插件通过 CPA 管理接口读取账号列表和指定凭证。
      probe_base_url: http://127.0.0.1:8317
      probe_management_key: "REPLACE-WITH-CPA-MANAGEMENT-KEY"

      # 可选固定出口；留空时直接连接。
      probe_proxies: []

      harvest_inband: false
      log_decisions: true
```

完整示例见 [`config.on-demand.example.yaml`](config.on-demand.example.yaml)。

### 按需设置

| 配置项 | 默认值 | 范围 | 说明 |
|---|---:|---:|---|
| `on_demand` | `false` | — | 开启业务请求到达时检查、获取和注入票 |
| `on_demand_accounts` | `[]` | — | 需要保护的 CPA 凭证完整文件名 |
| `on_demand_models` | `[]` | — | 需要保护的实际上游模型 |
| `on_demand_max_attempts` | `10` | 1–50 | 单次获取最多请求上游次数 |
| `on_demand_timeout_seconds` | `90` | 1–90 秒 | 单次获取总时间兜底 |

次数和总时间共同限制一次获取，哪个先达到就停止。客户端自己的重试会产生新的业务
请求，每次请求都有独立上限。

按需模式要求 `role: business` 和 `dry_run: false`。`inject_mode` 在按需模式下不会阻止
有效票注入。

## 看板

插件看板路径：

```text
/v0/resource/plugins/codex-turn-state/dashboard
```

在看板中可以：

- 开启或关闭按需打票；
- 选择 CPA 当前未停用的账号；
- 选择需要保护的模型；
- 设置单次最多尝试次数和兜底时间；
- 保存固定出口代理并测试连通性；
- 对指定账号和模型立即打票；
- 查看每次业务用票和手动打票结果。

看板只显示 `gpt-5.6-sol` 和 `gpt-6-astra` 作为按需保护模型。
`codex-auto-review`、`gpt-5.6-luna` 等未选择模型保持 CPA 原有行为。

### 立即打票与强制打票

- **立即打票**：遵守账号当前的本地冷却；已有有效票时直接复用；
- **强制打票**：绕过本地冷却，但仍遵守配置的次数和时间上限。

手动打票和业务缺票获取使用同一组账号、模型、代理和停止条件。

## 结果记录

“最近用票记录”会明确说明本次是否真的请求了上游：

```text
沿用未过期的 292 票并放行；本次打票尝试 0 次（未请求上游）
没有取得有效 292 票；本次打票尝试 4 次，292×0，312×3，网络失败×1
成功取得并保存 292 票；本次打票尝试 2 次，292×1，312×1
```

记录会分别统计：

- 292；
- 312；
- HTTP 401、403、429；
- HTTP 200 但没有 turn-state 请求头；
- 其他 HTTP 返回；
- 网络或代理传输失败。

记录只保存在当前插件进程内，最多 50 条；重启 CPA 后清空。票值不会显示在页面或
日志中。

## 上游错误处理

| 上游结果 | 插件处理 |
|---|---|
| 292 | 保存到当前账号和模型的桶，结束获取 |
| 312 | 不保存，仍有出口和预算时继续 |
| 401 | 标记账号需要重新授权，普通获取停止 |
| 403 | 账号进入冷却；操作员仍可手动强制尝试 |
| 429 | 账号进入冷却，避免继续加重限速 |
| 网络失败 | 计入本次统计，仍有预算时尝试下一个出口 |

按需获取失败或账号正在冷却时返回 **503**；达到总等待时间返回 **504**。插件不会在
没有有效 292 的情况下放行受保护的业务请求。

## 代理

`probe_proxies` 一行对应一个固定出口，按顺序尝试：

```text
socks5://user:password@host:port
http://user:password@host:port
https://user:password@host:port
```

代理必须包含 scheme。用户名或密码中的 `@`、`:`、`/`、`#` 需要 URL 编码。
看板的连通性测试不携带账号凭证，也不会产生票。

## 票的存储

```text
<store_dir>/
  index.json
  <账号凭证文件名>/
    <模型>.json
```

桶键由账号凭证文件名和实际模型组成。插件只接受长度为 292 的票，使用临时文件加
`rename` 原子写入；目录权限为 `0700`，文件权限为 `0600`。

看板修改的设置保存在 `<store_dir>/runtime.json`，重启后继续生效，并覆盖 YAML 中的
`role`、`dry_run` 和按需设置。

## 被动保存与旧模式

关闭按需模式时，插件可以从正常业务响应中被动保存有效 292，也可以使用原有主动探测
模式填充和续期票池。开启按需模式后，后台主动探测停止，受保护请求缺票时改为同步
获取，空闲账号保持安静。

旧模式配置见 [`config.example.yaml`](config.example.yaml)，turn-state 格式和时间戳
研究见 [`FINDINGS.md`](FINDINGS.md)。

## 安全说明

- 插件看板及 `/ops/*` 位于 CPA 的 resource 路径，本插件不提供额外页面密码；应由
  CPA、CPAMP 或反向代理入口负责鉴权。
- `probe_management_key` 不会由状态接口返回，也不会写入页面或日志。
- 状态接口会向看板返回代理配置以便编辑，不要把插件 resource 路径暴露给不可信用户。
- CPA 升级后如果内部请求头处理顺序改变，需要重新验证真正发往上游的
  `X-Codex-Turn-State`，不能只看插件的 `inject` 日志。
- 插件被禁用、熔断或加载失败时无法拦截业务请求。开启按需模式前应确认插件已加载并
  注册成功。

## License

MIT
