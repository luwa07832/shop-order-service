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

提交单个订单对象：`tenant_id`、`idempotency_key` 为非空且不全为空白的字符串（首尾空白原样保留），`lines` 为非空数组。每行包含 `product_id`（同样为非空白字符串）、`quantity`（正整数）、`unit_price`（非负整数），均为有符号 64 位整数，金额以分计。商品与价格按请求快照保存，行顺序即提交顺序；额外字段忽略。

```json
{
  "tenant_id": "tenant-a",
  "idempotency_key": "cart-42",
  "lines": [
    {"product_id": "sku-1", "quantity": 2, "unit_price": 150},
    {"product_id": "sku-2", "quantity": 1, "unit_price": 0}
  ]
}
```

- 首次创建返回 **HTTP 201**，订单含 `id`、`tenant_id`、`idempotency_key`、`lines`、`total_amount`（各行数量乘单价之和）、`status`（固定 `"created"`）、`created_at`（UTC RFC3339）。
- 同租户以相同键重提**相同内容**（JSON 字段顺序与空白不影响比较，行顺序参与比较）返回 **HTTP 200** 与原订单，`id`、`created_at` 与快照一致；并发相同提交只创建一个订单。
- 相同键但**内容不同**返回 **HTTP 409** `idempotency_conflict`，原订单不变。
- 幂等键按租户隔离，不同租户可复用同一键。
- 非法 JSON、字段缺失或类型错误、空行数组、无效数量/单价、合计超出 int64 范围，返回 **HTTP 400** `invalid_request`。
- 存储不可用返回 **HTTP 503** `storage_unavailable`，失败的创建不会留下可查询订单。

### `GET /v1/orders/:id?tenant_id=...`

按租户查询订单明细。匹配返回 **HTTP 200** 与订单对象；订单不存在、属于其他租户、缺少或空白的 `tenant_id` 分别返回 **HTTP 404** `order_not_found`（前两种）与 **HTTP 400** `invalid_request`（租户标识无效）。

订单主体与金额快照持久保存在 SQLite 中，重开存储后查询与幂等重提结果保持一致。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
