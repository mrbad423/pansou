# 自动过滤失效链接与 GitHub Docker 构建

本 Fork 在原有 `GET/POST /api/search` 中默认加入链接检测和失效过滤。原有请求不需要增加参数。

## 行为与限制

1. 搜索和原有关键词过滤完成后，对所有返回链接读取有效检测缓存。
2. 移除缓存中明确为 `bad` 的链接；没有缓存的链接优先选取 20 个去重身份进入检测队列。
3. 默认最多额外等待 2000ms；已完成且明确为 `bad` 的链接也会被移除。
4. 超时、验证码、限流、密码错误、不支持检测、超出检测限额的链接保留，不冒充“验证有效”。
5. 已提交的未完成任务可在后台继续运行，结果供后续请求使用，不改变已经返回的响应。

**2000ms 限制的是搜索完成后的检测等待，不包含原搜索、结果遍历、磁盘缓存读取及 JSON 编码。** 不保证所有链接在首次返回前完成检测。

自动检测共享 8 个工作者，同一网盘最多 2 个正在执行的任务；待执行和运行中的任务合计最多 100 个。同一链接、密码身份的并发任务会合并；队列满时直接保留未验证链接。每个任务从入队算起最多存活 12 秒。

`/api/check/links` 手动检测接口继续可用，并复用同一个检测服务、缓存和同身份请求合并机制；其现有批量执行方式不变。上述 8 个工作者限制针对搜索自动检测队列，不是整个应用所有出站请求的总限制。

## 在 GitHub 构建镜像

提交本次代码和 `.github/workflows/docker_ci.yml` 到你 Fork 的默认分支 `main`，或在 Actions 中手动运行 **Test and publish PanSou Docker images**。

工作流会：

- 运行 API、service 和 model 包的 Go 测试及竞态检测，并编译所有包。
- 为两种镜像构建 amd64 测试镜像，在 Actions runner 上启动容器检查 `/api/health`。
- 检查通过后，构建 `linux/amd64` 和 `linux/arm64`，发布到 GitHub Container Registry。
- 发布默认分支的 `latest`、分支标签和 `sha-...` 标签。Pull Request 只验证，不推送镜像。

你的仓库预期生成：

```text
ghcr.io/mrbad423/pansou:latest       # 纯后端，8888 端口
ghcr.io/mrbad423/pansou-web:latest   # 网页 + Nginx + 定制后端，80 端口
```

工作流使用仓库自动提供的 `GITHUB_TOKEN` 和 `packages: write`，不再需要原工作流中的 `DOCKER` Secret。若 GitHub 策略限制写包权限，需要在仓库/组织 Actions 设置中允许；若同名 Package 已存在，检查其 Actions 仓库访问权限。

首次发布后在 GitHub Packages 检查可见性：无登录拉取和后续 Unikraft 构建通常需要 Public；Private 则需由部署端配置拉取凭据。

`Dockerfile.web` 以官方 `ghcr.io/fish2018/pansou-web:latest` 为基础，保留网页、Nginx、启动脚本及默认插件/频道，只替换编译后的 `/app/pansou`。需要固定上游版本时设置构建参数 `PANSOU_WEB_BASE` 为具体 tag 或 digest。

本地文件准备好不代表 GitHub 已收到或镜像已经生成；以 Actions 的成功运行及 Packages 中的镜像为准。

## 请求示例

保持现有调用，默认自动检测并过滤：

```bash
curl --get 'https://你的域名/api/search' \
  --data-urlencode 'kw=搜索关键词'
```

跳过检测，返回原搜索行为：

```bash
curl --get 'https://你的域名/api/search' \
  --data-urlencode 'kw=搜索关键词' \
  --data-urlencode 'check_links=false'
```

只使用缓存过滤，不新增检测任务，也不等待网络：

```json
{
  "kw": "搜索关键词",
  "check_limit": 0
}
```

显式配置检测，只标记不移除：

```json
{
  "kw": "搜索关键词",
  "check_links": true,
  "filter_invalid": false,
  "check_limit": 20
}
```

三个参数均支持 GET 查询参数和 POST JSON。`check_limit` 的允许范围为 0..100，非法值返回 HTTP 400。检测等待时间只能由服务端环境变量配置，客户端不能任意加长。原有鉴权继续生效，示例在启用鉴权时需要按原方式携带 Token。

## 响应兼容

保留外层 `code`、`message`、`data`，保留链接原字段。`data.link_check` 增加本次检测汇总，每个返回链接的 `link_check` 增加状态信息。

示意：

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "total": 1,
    "merged_by_type": {
      "quark": [{
        "url": "https://pan.quark.cn/s/example",
        "password": "",
        "note": "示例",
        "datetime": "2026-10-25T00:00:00Z",
        "link_check": {
          "disk_type": "quark",
          "url": "https://pan.quark.cn/s/example",
          "state": "unchecked",
          "cache_hit": false,
          "checked_at": 0,
          "expires_at": 0,
          "summary": "未完成检测：数量、队列或时间预算限制"
        }
      }]
    },
    "link_check": {
      "total_links": 2,
      "checked": 1,
      "cache_hits": 1,
      "filtered": 1,
      "unchecked": 1,
      "uncertain": 0,
      "unsupported": 0,
      "duration_ms": 2000,
      "budget_exhausted": true
    }
  }
}
```

汇总按去重后的“网盘 + 规范化 URL + 提取码”身份计数，包含被移除的链接；同一链接在 `results` 与 `merged_by_type` 同时出现只计一次。`checked` 包含已得到 `uncertain` 或 `unsupported` 的结果，不表示“有效链接数”。

状态：`ok` 有效；`bad` 明确失效；`locked` 需要提取码/密码错误；`uncertain` 无法确认；`unsupported` 不支持；`unchecked` 未完成检测。默认只有 `bad` 被移除。

`res=merge`/`merged_by_type` 的 `total` 是过滤后合并链接数；`res=results`/`all` 的 `total` 保持为过滤后的搜索记录数。记录中所有链接均被移除时，记录也移除；`all` 模式同步过滤两种结构。原始搜索缓存不附加检测状态，不受某次请求的过滤开关影响。

常规 JSON 客户端可继续读取原字段；如果客户端严格拒绝未知字段，需允许新增的 `link_check` 字段，或先以 `check_links=false` 保持原行为。

## 服务端参数

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SEARCH_CHECK_LINKS` | `true` | 是否默认启用；请求可显式覆盖 |
| `SEARCH_FILTER_INVALID` | `true` | 是否默认过滤 `bad`；请求可显式覆盖 |
| `SEARCH_CHECK_LIMIT` | `20` | 每次新增/加入的检测身份上限，范围 0..100 |
| `SEARCH_CHECK_BUDGET_MS` | `2000` | 额外等待预算，范围 0..5000ms；0 只读取缓存 |
| `CACHE_PATH` | `./cache` | 检测缓存数据库目录；web 镜像为 `/app/data/cache` |

无效的环境变量值回退到默认值。设置全局关闭后，如请求显式传 `check_links=true`，仍允许检测；它是默认值配置，不是禁用检测功能的访问控制开关。

缓存有效期：有效 30 分钟；失效 10 分钟；需要提取码 5 分钟；不确定 30 秒；不支持 24 小时。内存与持久化检测缓存均限制为最多 4096 条。缓存键包含密码和代理身份，并使用版本前缀避免沿用旧版宽松失效判断。过期结果会重新检查，缓存不代表链接永远有效或失效。

## 后续自行部署

网页集成版使用 `ghcr.io/mrbad423/pansou-web:latest`；保留 `/app/data` 数据卷挂载和 80 端口入口。原有 Unikraft 工作流需要把 `PANSOU_SOURCE_IMAGE` 改为该镜像，再自行构建、推送和部署。当前交付不自动执行 Unikraft 部署。

## 验证

```bash
go test ./api ./service ./model
go vet ./api ./service ./model
go test -race ./api ./service ./model
go build ./...
```

测试使用模拟上游响应，不依赖实际网盘可用性，覆盖：GET/POST 参数、缓存过滤、原结构与缓存不被修改、合并与原始结果总数、检测截止时间、后台补齐、全局/网盘并发上限、队列容量、相同身份去重、密码/代理隔离、持久化重开、取消传递，以及网络异常/未知错误不误删。
