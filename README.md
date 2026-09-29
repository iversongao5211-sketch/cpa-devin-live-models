# cpa-devin-live-models

CLIProxyAPI 原生插件（`.so`，C ABI v1）：**第一时间支持 Devin/Cognition 新发布的模型**。

## 它解决什么

CPA 内建的 devin 模型目录来自编译时内嵌的 `devin_models.json` + 上游维护的策展 JSON（3h 轮询，`--local-model` 下完全关闭）。Cognition 发布新模型（如 `claude-sonnet-5-5`、`gpt-6-1-sol`）后，CPA 要等策展更新或重编才能路由——通常落后数小时到数天。

本插件周期性地**用一个池内 devin 账号直接调上游** `POST https://server.codeium.com/exa.api_server_pb.ApiServerService/GetCliModelConfigs`（Connect-RPC），拿到账号视角的真实模型清单，与内嵌基线 + 策展 JSON 做并集，通过 `model.for_auth`/`model.static` 回调注册到每个 devin 凭据上。目录变化时改写 auth 文件中的 `_devin_live_rev` 标记，触发宿主 fs watcher 重注册——**新模型在上游上线后一个轮询周期内即可出现在 `/v1/models` 并可被路由**。

## 安装

### 方式 A：本地落盘
1. `cpa-devin-live-models-v1.0.0.so` → `plugins/linux/amd64/`（或对应平台目录）
2. `config.yaml` 的 `plugins.configs` 加：
   ```yaml
   plugins:
     enabled: true
     configs:
       cpa-devin-live-models:
         enabled: true
   ```
3. 触发一次配置热加载（改 config / PUT /v0/management/config）或重启

### 方式 B：插件商店（github-release）
在 `plugins.store-sources` 加入：
```yaml
- https://raw.githubusercontent.com/szxypi/cpa-devin-live-models/main/registry.json
```
然后通过管理面板/管理 API 安装。

## 配置项（`plugins.configs.cpa-devin-live-models`）

| 键 | 默认 | 说明 |
|---|---|---|
| `enabled` | true | 总开关（false 时目录回退到宿主内建行为） |
| `poll-interval` | `5m` | 上游拉取周期（≥15s） |
| `fetch-timeout` | `30s` | 单次上游调用超时（3s–120s） |
| `accounts-per-poll` | `3` | 每轮最多轮换使用几个 devin 账号做并集 |
| `propagate-on-change` | `true` | 目录变化时 touch auth 文件强制重注册（写 `_devin_live_rev` 标记字段） |
| `emit-variants` | `true` | 额外注册 `devin/<model>-<level>` 努力值变体条目 |
| `curated-urls` | router-for-me 两个官方源 | 附加策展 JSON 源；置 `[]` 关闭 |
| `proxy-url` | `""` | auth 文件无 `proxy_url` 时的兜底代理（支持 socks5/http） |
| `baseline-file` | `""` | 外部基线 JSON 覆盖内嵌拷贝 |
| `log` | `true` | 轮询日志 |

## 管理 API

- `GET /v0/management/plugins/cpa-devin-live-models/status` — 目录规模/各源计数/最近轮询/错误
- `GET /v0/management/plugins/cpa-devin-live-models/catalog` — 当前合并后的模型 id 列表
- `POST /v0/management/plugins/cpa-devin-live-models/refresh` — 立即轮询一次

## 行为细节

- 模型暴露：活目录经过宿主 `oauth-excluded-models` 与 per-auth `excluded_models` 过滤（与内建路径语义一致）；上游新发模型默认自动暴露。
- 拉取只读：每轮用 ≤`accounts-per-poll` 个健康 devin 凭据做一次 unary 调用；失败保留上次成功目录；不打 cooldown、不计入数据面结果。
- 与 `--local-model` 无关：插件自管轮询，不受远端目录更新开关影响。
- Home 模式（无 fs watcher）下 propagate touch 静默无效，模型仍会随下一次 auth 注册事件生效。

## 构建

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm sh build.sh
# 产物：cpa-devin-live-models-v1.0.0.so + .h
```

兼容 CPA ≥ v7.3.16 与 v8.x（ABI v1 / schema v6）。
