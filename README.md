# EH Blacklist Server

一个给 ExHentai / E-Hentai userscript 使用的纯 API 黑名单后端。

- Go + SQLite，单二进制，无前端页面
- 支持 ID 黑名单、关键词黑名单、原因字典
- 批量判定、完整导出/导入、旧格式兼容
- 可选 Bearer Token 鉴权、可配置 CORS、请求大小与批量上限
- 与旧版数据库完全兼容，无需迁移

## 目录结构

```text
main.go                     # go run . 入口
cmd/server/main.go          # go run ./cmd/server 入口（兼容旧布局）
internal/blacklist/
  model.go                  # 数据结构
  normalize.go              # 文本/ID/原因标准化
  index.go                  # 内存索引 + Aho-Corasick 关键词匹配
  store.go                  # SQLite、CRUD、导入导出
  http.go                   # 路由、鉴权、CORS、限制、日志
  config.go                 # 环境变量
  run.go                    # 启动与优雅退出
data/blacklist.db           # 运行时生成的 SQLite 数据库
```

## 运行

要求 Go 1.22 或更新版本。

```bash
go run .
# 或者
go run ./cmd/server
```

默认监听 `:8787`，数据库为 `./data/blacklist.db`。在 IDEA / GoLand 中直接运行
`main.go` 或 `cmd/server/main.go` 均可。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `:8787` | HTTP 监听地址 |
| `DB_PATH` | `./data/blacklist.db` | SQLite 文件路径，目录会自动创建 |
| `API_TOKEN` | 空 | 非空时启用鉴权；除 `/health` 和 CORS 预检外都需要 Token |
| `CORS_ORIGINS` | 空（等价 `*`） | 逗号分隔的允许来源；支持精确来源或 `*` |
| `MAX_CHECK_ITEMS` | `1000` | 单次 `/check` 允许的最大条目数 |
| `MAX_BODY_BYTES` | `67108864`（64 MiB） | 请求体上限，超出返回 413 |
| `TRUST_PROXY` | `false` | 是否信任 `X-Forwarded-For` / `X-Real-IP`，仅在反向代理后开启 |

## 安全

默认保持旧版行为：**无鉴权**，适合本机或内网使用。只要端口暴露到公网，建议至少设置：

```bash
export API_TOKEN='a-long-random-token'
export CORS_ORIGINS='https://example.com'
```

之后健康检查以外的请求都需要：

```bash
curl -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8787/stats
```

也接受 `X-API-Token` 头。Token 使用 SHA-256 摘要 + 常量时间比较；`/health`
保持公开，方便 Docker healthcheck。`TRUST_PROXY` 默认关闭，避免客户端伪造
`X-Forwarded-For` 污染日志；确认自己位于可信反向代理之后再打开。

服务本身不提供 TLS。公网部署请放在 Caddy / Nginx 等反向代理之后，并只监听
内网地址。

## Docker

```bash
docker compose up -d --build
curl http://127.0.0.1:8787/health
```

数据保存在 `./data/blacklist.db`。`docker compose down` 不会删除数据；重新
`up` 即可继续使用。

镜像构建为多阶段，运行镜像包含 `ca-certificates`、`tzdata` 和 healthcheck。
GitHub Actions 会在推送 `v*.*.*` tag 或手动触发时构建 `linux/amd64`、
`linux/arm64` 镜像并推送到 Docker Hub（见 `.github/workflows/dockerhub.yml`）。

## 旧数据库兼容

数据库 schema 与原版完全一致：`blacklist_entries`、`blacklist_reasons` 两张表，
列名、主键 `(type, value_norm)`、索引都没有变化。启动时只执行
`CREATE TABLE IF NOT EXISTS`，不会修改或重建旧表，因此可以直接把
`DB_PATH` 指向原来的 `data/blacklist.db`。

- 旧库中的 ID、关键词、原因、时间戳会被原样读取
- 旧库缺失的内置原因会被补齐，已有原因不会被覆盖
- 读取时会正常处理 SQLite WAL 文件
- 自动化测试 `TestLegacyDatabaseCompatibility` 会用旧 schema 建库、写入数据，
  再用新版打开、查询、写入并重新打开验证

备份建议使用 `GET /export` 做逻辑备份；如果要直接复制文件，请先停止服务，
并同时复制 `blacklist.db`、`blacklist.db-wal`、`blacklist.db-shm`。

## API

除特别说明外，响应都是 JSON。设置 `API_TOKEN` 后，除 `/health` 外的请求都要带
`Authorization: Bearer <token>`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 健康检查，公开 |
| GET | `/stats` | ID / 关键词 / 原因数量 |
| POST | `/check` | 批量判定 |
| GET | `/entries` | 列表，支持 `type`、`q`、`limit`、`offset` |
| POST/PUT | `/entries` | 新增或覆盖条目 |
| DELETE | `/entries?type=id&value=123` | 按 value 删除（自动标准化） |
| PUT/DELETE | `/entries/{type}/{value_norm}` | 按路径更新/删除 |
| GET/POST | `/reasons` | 原因列表 / 新增原因 |
| PUT/DELETE | `/reasons/{code}` | 更新/删除原因 |
| GET | `/export` | 导出完整 v2 JSON |
| POST/PUT | `/import?overwrite=true\|false` | 导入 v2 或旧版格式 |

删除不存在的条目或原因返回 `404`；参数错误返回 `400`；未授权返回 `401`；
请求体过大返回 `413`。

### 健康检查与统计

```bash
curl http://127.0.0.1:8787/health
curl -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8787/stats
```

### 新增条目

```bash
curl -X POST http://127.0.0.1:8787/entries \
  -H "Authorization: Bearer $API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "id",
    "value": "123456",
    "reason_code": "artist",
    "priority": 100
  }'
```

`reason_label` 可以省略：如果 `reason_code` 存在于原因字典，服务会自动填上对应
标签。关键词同理：

```bash
curl -X POST http://127.0.0.1:8787/entries \
  -H "Authorization: Bearer $API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"type":"keyword","value":"测试关键词","reason_code":"content"}'
```

### 批量判定

```bash
curl -X POST http://127.0.0.1:8787/check \
  -H "Authorization: Bearer $API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "items": [
      {"gid": "123456", "title": "任意标题"},
      {"gid": "999999", "title": "这是一个测试关键词标题"}
    ]
  }'
```

响应示例：

```json
{
  "items": [
    {
      "gid": "123456",
      "blacklisted": true,
      "match": {
        "type": "id",
        "value": "123456",
        "value_norm": "123456",
        "reason_code": "artist",
        "reason_label": "不喜欢作者",
        "priority": 100
      }
    },
    {
      "gid": "999999",
      "blacklisted": true,
      "match": {
        "type": "keyword",
        "value": "测试关键词",
        "value_norm": "测试关键词",
        "reason_code": "content",
        "reason_label": "题材避雷",
        "priority": 100
      }
    }
  ]
}
```

未命中时 `blacklisted` 为 `false`，`match` 为 `null`。单次请求超过
`MAX_CHECK_ITEMS` 会返回 `400`。

### 查询、更新、删除条目

```bash
# 列表：type 可选 id/keyword，q 模糊搜索
curl -H "Authorization: Bearer $API_TOKEN" \
  'http://127.0.0.1:8787/entries?type=keyword&limit=50&offset=0'

# 按 value 删除（服务端会做关键词标准化）
curl -X DELETE -H "Authorization: Bearer $API_TOKEN" \
  'http://127.0.0.1:8787/entries?type=keyword&value=测试关键词'

# 按路径更新
curl -X PUT -H "Authorization: Bearer $API_TOKEN" \
  -H 'Content-Type: application/json' \
  'http://127.0.0.1:8787/entries/keyword/测试关键词' \
  -d '{"reason_code":"style","priority":50}'
```

### 原因字典

```bash
curl -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8787/reasons

curl -X POST -H "Authorization: Bearer $API_TOKEN" \
  -H 'Content-Type: application/json' \
  http://127.0.0.1:8787/reasons \
  -d '{"code":"custom-a","label":"自定义原因","sort_order":80}'
```

修改原因标签会同步更新引用该原因的所有条目；删除原因会把条目的
`reason_code` 置空，但保留最后一次 `reason_label` 作为历史快照。

### 导出

```bash
curl -H "Authorization: Bearer $API_TOKEN" \
  http://127.0.0.1:8787/export > blacklist-export.json
```

导出**不再有 5000 条上限**，会返回全部条目；响应带
`Content-Disposition: attachment`。

### 导入

默认覆盖导入：

```bash
curl -X PUT -H "Authorization: Bearer $API_TOKEN" \
  'http://127.0.0.1:8787/import?overwrite=true' \
  -H 'Content-Type: application/json' \
  --data-binary @blacklist-export.json
```

合并导入：

```bash
curl -X PUT -H "Authorization: Bearer $API_TOKEN" \
  'http://127.0.0.1:8787/import?overwrite=false' \
  -H 'Content-Type: application/json' \
  --data-binary @blacklist-export.json
```

仍兼容旧 userscript 的 `{ids: [], keywords: []}` 格式：

```bash
curl -X PUT -H "Authorization: Bearer $API_TOKEN" \
  'http://127.0.0.1:8787/import?overwrite=false' \
  -H 'Content-Type: application/json' \
  -d '{"ids":["123456"],"keywords":["测试关键词"]}'
```

覆盖导入在同一个事务中执行：如果 payload 校验失败，原有数据不会被删除。

如果导出的文件超过 `MAX_BODY_BYTES`（默认 64 MiB），导入会返回 `413`；
导入前按需调高该环境变量即可。

## 匹配规则

1. 先匹配 ID；命中立即返回，不再检查关键词。
2. ID 未命中时，在标题中查找关键词子串。
3. 关键词顺序：`priority ASC, created_at ASC, value_norm ASC`，只返回最优的一条。
4. 标题和关键词都会做标准化：转小写、去空白、全角标点转半角并删除标点。
5. 关键词匹配使用 Aho-Corasick 自动机，复杂度约为
   `O(标题长度 + 命中数)`，不再随关键词总数线性增长。
6. 单条标题最多处理 4096 个字符，避免异常输入放大 CPU 开销。

## Userscript 调用建议

列表页一次性收集作品：

```js
const items = [...document.querySelectorAll('.itg .gl1t')].map(row => ({
  gid: row.dataset.ehblGid,
  title: row.dataset.ehblTitle
}));
```

批量请求（未设置 `API_TOKEN` 时删除 `Authorization` 头）：

```js
GM_xmlhttpRequest({
  method: 'POST',
  url: 'http://127.0.0.1:8787/check',
  headers: {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer YOUR_TOKEN'
  },
  data: JSON.stringify({ items }),
  onload: res => {
    const data = JSON.parse(res.responseText);
    console.log(data);
  }
});
```

## 开发与测试

```bash
go test ./...
go vet ./...
```

测试覆盖：旧数据库兼容、导出完整性、导入事务回滚、原因级联、匹配优先级、
Aho-Corasick 与朴素扫描的随机对拍、鉴权、CORS、请求上限和完整 CRUD 流程。
