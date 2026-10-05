# shop-order-service

把多租户订单的下单请求、幂等键、状态迁移、支付回执与取消记录成可查询的服务，支持按租户和订单号读取订单明细，并追溯每一次状态变更与最终支付结果。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `shop-order-service.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /v1/orders`

创建订单。请求体为单个 JSON 对象：

```json
{
  "tenant_id": "tenant-a",
  "idempotency_key": "key-1",
  "lines": [
    {"product_id": "p-1", "quantity": 2, "unit_price": 500}
  ]
}
```

- `tenant_id`、`idempotency_key`、`product_id`：非空且不全为空白的字符串，首尾空白原样保留。
- `quantity`：正的 64 位整数；`unit_price`：非负的 64 位整数，金额以分计。
- `lines` 至少一行，订单行按提交顺序保存；未知字段被忽略。

首次创建返回 HTTP 201，订单对象额外包含 `id`（不透明订单号）、`total_amount`（各行数量乘单价之和）、`status`（固定 `created`）与 `created_at`（UTC，RFC3339）。

同一租户内幂等键唯一：以相同键与相同内容重提返回 HTTP 200 及原订单（JSON 字段顺序与空白不影响比较，订单行顺序参与比较）；内容不同返回 HTTP 409 与 `idempotency_conflict`，原订单不变。不同租户可复用同一键。

### `GET /v1/orders/:id?tenant_id=...`

按租户查询订单明细。匹配返回 HTTP 200 及订单对象；订单不存在或属于其他租户返回 HTTP 404 与 `order_not_found`；缺少或无效的 `tenant_id` 返回 HTTP 400 与 `invalid_request`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
