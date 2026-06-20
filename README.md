# EH Blacklist Server

一个给 ExHentai / E-Hentai userscript 使用的纯 API 黑名单后端。

特性：

- Go + SQLite
- Docker 可部署
- 纯 API，无前端页面
- 支持 ID 黑名单和关键词黑名单
- 支持黑名单原因
- 支持常用原因
- 支持批量查询
- 匹配规则：ID 优先短路；关键词只返回第一个命中
- 支持导入旧 userscript 的 `{ ids: [], keywords: [] }` 格式
- 支持导出新版 JSON

## IDEA / GoLand 调试运行

1. 用 IntelliJ IDEA Ultimate + Go 插件，或 GoLand，打开本项目目录。
2. 等待 IDE 执行 `go mod download`。
3. 运行入口：

```text
cmd/server/main.go
```

默认监听：

```text
:8787
```

默认数据库：

```text
./data/blacklist.db
```

可选环境变量：

```text
ADDR=:8787
DB_PATH=./data/blacklist.db
```

命令行运行：

```bash
go run ./cmd/server
```

## Docker 运行

```bash
docker compose up -d --build
```

数据会保存在：

```text
./data/blacklist.db
```

## API 示例

### 健康检查

```bash
curl http://127.0.0.1:8787/health
```

### 统计

```bash
curl http://127.0.0.1:8787/stats
```

### 新增 ID 黑名单

```bash
curl -X POST http://127.0.0.1:8787/entries \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "id",
    "value": "123456",
    "reason_code": "artist",
    "reason_label": "不喜欢作者",
    "priority": 100
  }'
```

### 新增关键词黑名单

```bash
curl -X POST http://127.0.0.1:8787/entries \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "keyword",
    "value": "测试关键词",
    "reason_code": "content",
    "reason_label": "题材避雷",
    "priority": 100
  }'
```

### 批量查询

```bash
curl -X POST http://127.0.0.1:8787/check \
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

### 查询条目

```bash
curl 'http://127.0.0.1:8787/entries?type=keyword&limit=50&offset=0'
```

### 删除条目

按 value 自动标准化删除：

```bash
curl -X DELETE 'http://127.0.0.1:8787/entries?type=id&value=123456'
```

或按 value_norm 删除：

```bash
curl -X DELETE 'http://127.0.0.1:8787/entries?type=keyword&value_norm=测试关键词'
```

### 常用原因

```bash
curl http://127.0.0.1:8787/reasons
```

新增常用原因：

```bash
curl -X POST http://127.0.0.1:8787/reasons \
  -H 'Content-Type: application/json' \
  -d '{"code":"custom-a","label":"自定义原因","sort_order":80}'
```

### 导出

```bash
curl http://127.0.0.1:8787/export > blacklist-export.json
```

### 导入新版格式

默认覆盖导入：

```bash
curl -X PUT 'http://127.0.0.1:8787/import?overwrite=true' \
  -H 'Content-Type: application/json' \
  --data-binary @blacklist-export.json
```

合并导入：

```bash
curl -X PUT 'http://127.0.0.1:8787/import?overwrite=false' \
  -H 'Content-Type: application/json' \
  --data-binary @blacklist-export.json
```

### 导入旧 userscript 格式

```json
{
  "ids": ["123456"],
  "keywords": ["测试关键词"]
}
```

```bash
curl -X PUT 'http://127.0.0.1:8787/import?overwrite=false' \
  -H 'Content-Type: application/json' \
  -d '{"ids":["123456"],"keywords":["测试关键词"]}'
```

## 匹配规则

1. 先匹配 ID。
2. ID 命中后立即返回，不再检查关键词。
3. ID 未命中时，按关键词优先级匹配。
4. 关键词排序规则：`priority ASC, created_at ASC, value_norm ASC`。
5. 只返回第一个命中的关键词。
6. 不返回多个 matches，只返回单个 `match`。

## userscript 调用建议

列表页一次性收集所有作品：

```js
const items = [...document.querySelectorAll('.itg .gl1t')].map(row => ({
  gid: row.dataset.ehblGid,
  title: row.dataset.ehblTitle
}));
```

批量请求：

```js
GM_xmlhttpRequest({
  method: 'POST',
  url: 'http://127.0.0.1:8787/check',
  headers: { 'Content-Type': 'application/json' },
  data: JSON.stringify({ items }),
  onload: res => {
    const data = JSON.parse(res.responseText);
    console.log(data);
  }
});
```
