# TUNVN Shop Bot ⇄ Dujiao-Next Adapter

把 TUNVN Shop Bot（越南自动发卡站）包装成 Dujiao-Next 的**上游站点（B 站）**，
让你的独立站（A 站）可以直接「同步商品 → 客户下单 → 自动采购 → 自动发货」，
零囤货赚差价。

[![build-and-publish](https://github.com/PobENo17/tunvn-dujiao-adapter/actions/workflows/publish.yml/badge.svg)](https://github.com/PobENo17/tunvn-dujiao-adapter/actions/workflows/publish.yml)

```bash
docker pull ghcr.io/pobeno17/tunvn-dujiao-adapter:latest
```

```
客户 ──▶ 你的 Dujiao-Next 站 (A)
              │  POST /api/v1/upstream/orders   (HMAC-SHA256 签名)
              ▼
        tunvn-adapter  (本服务，扮演 B 站)
              │  POST /api/buy   (X-API-Key)
              ▼
        TUNVN Shop Bot  ──► 返回账号/卡密
              │
              ▼  签名回调 POST /api/v1/upstream/callback
        A 站订单变为 delivered，客户看到卡密
```

## 为什么需要这一层

Dujiao-Next 只支持 `dujiao-next` 一种上游协议（`/api/v1/upstream/*` + HMAC 签名 + **异步回调**交付），
而 TUNVN 是 `X-API-Key` + **同步返回**卡密。协议、鉴权、交付模型三者都不同，无法直连。

## 已实现的接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/upstream/ping` | 连通性 + TUNVN 钱包余额 |
| GET | `/api/v1/upstream/categories` | 返回空列表（TUNVN 无分类，Dujiao 自动降级） |
| GET | `/api/v1/upstream/products` | 商品列表（分页，支持 `include_inactive`/`updated_after` 忽略） |
| GET | `/api/v1/upstream/products/:id` | 单品详情 |
| POST | `/api/v1/upstream/orders` | 创建采购单（**幂等**，扣 TUNVN 余额） |
| GET | `/api/v1/upstream/orders/:id` | 订单详情 + 已交付卡密（回调的兜底） |
| POST | `/api/v1/upstream/orders/:id/cancel` | 已付款单一律拒绝（`cancel_not_allowed`） |
| POST | `/api/v1/upstream/callback` | **由你的站点实现**，本服务向它推送交付 |

另有一个只读的本地诊断端点 `GET /admin/orders?limit=50`（无鉴权，务必只绑定 loopback）。

## 商品映射规则

TUNVN 没有 SKU，所以每个商品导出一个 `DEFAULT` SKU，**SKU ID 就等于 TUNVN 的商品 ID**。
`fulfillment_type` 固定为 `auto`；`stock_quantity` 直接取 TUNVN 的 `stock`（负数表示无限）。

价格取 `price_usdt`（或 `price_vnd`），**向上取整到分且绝不为 0**，避免 0.004 USDT 的商品在
Dujiao（两位小数）里变成免费商品。

## 快速开始

### 1. 准备配置

```bash
cp .env.example .env
# 编辑 .env：
#   ADAPTER_API_KEY / ADAPTER_API_SECRET  -> 自己生成，稍后填到 Dujiao 后台
#   TUNVN_API_KEY                         -> 你的 Shop Bot API Key
openssl rand -hex 24   # 用来生成上面两个值
```

### 2. 在服务器上部署（拉取现成镜像）

镜像已发布到 GitHub Container Registry，支持 `linux/amd64` 与 `linux/arm64`：

```bash
docker pull ghcr.io/pobeno17/tunvn-dujiao-adapter:latest
```

推荐用 compose（`docker-compose.yml` 已指向该镜像）：

```bash
mkdir -p /opt/tunvn-adapter && cd /opt/tunvn-adapter
curl -fsSLO https://raw.githubusercontent.com/PobENo17/tunvn-dujiao-adapter/main/docker-compose.yml
cp .env.example .env 2>/dev/null || curl -fsSL https://raw.githubusercontent.com/PobENo17/tunvn-dujiao-adapter/main/.env.example -o .env
# 编辑 .env，填入 TUNVN_API_KEY 和自建的 ADAPTER_API_KEY/SECRET
docker compose up -d
docker compose logs -f
```

或纯 docker 命令：

```bash
docker run -d --name tunvn-adapter --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v /opt/tunvn-adapter/data:/data \
  -e TUNVN_API_KEY=你的key \
  -e ADAPTER_API_KEY=你生成的key \
  -e ADAPTER_API_SECRET=你生成的secret \
  -e TUNVN_CURRENCY=usdt \
  -e LISTEN_ADDR=0.0.0.0:8080 \
  -e DATA_FILE=/data/orders.json \
  ghcr.io/pobeno17/tunvn-dujiao-adapter:latest
```

升级：

```bash
docker compose pull && docker compose up -d
```

服务需要**公网 HTTPS**（Dujiao 会向它发请求，并在创建采购单时校验回调地址不能是内网）。
可用镜像标签：`latest`（main 分支）、`sha-<commit>`、以及打了 `v*` tag 时的 `1.2.3` / `1.2`。

建议 Nginx 反代示例：

```nginx
location /api/v1/upstream/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_read_timeout 60s;
}
```

> 不要暴露 `GET /admin/orders`（无鉴权诊断端点），它只应经 loopback 访问。

### 2b. 从源码构建（可选）

```bash
go build -o tunvn-adapter .
./tunvn-adapter
```

或本地构建镜像：

```bash
docker build -t tunvn-adapter:local .
```

### 3. 在 Dujiao 后台配置

`对接管理 → 连接管理 → 新增`：

| 字段 | 值 |
| --- | --- |
| 站点地址 | `https://adapter.你的域名` |
| 协议类型 | `dujiao-next`（默认） |
| API Key | `.env` 里的 `ADAPTER_API_KEY` |
| API Secret | `.env` 里的 `ADAPTER_API_SECRET` |
| 回调地址 | `https://你的商店域名/api/v1/upstream/callback` |
| 汇率 | 见下方「定价」 |
| 加价% | 你的毛利，例如 `60` |
| 取整 | `ceil_tenth` 或 `ceil_int` |
| 自动同步价格 | 建议开 |

点「测试连接」，应返回 TUNVN 的余额。

### 4. 同步商品并上架

`对接管理 → 商品映射 → 选连接 → 同步商品` → 勾选要卖的 → 导入。
**导入后商品默认下架**，需要手动上架并调整标题/描述（TUNVN 的说明里可能带外链，建议清理）。

### 5. 验证一单

先充 TUNVN 余额（`POST /api/deposit`，USDT BEP20，最少 1 USDT，必须按返回的
`exact_amount` 精确转账），然后用最便宜的商品（如 `MUSE AI 1B TOKEN` = 0.06 USDT）跑通一单。

## 定价（币种 USDT 时）

推荐 Adapter 侧用 `TUNVN_CURRENCY=usdt`，Dujiao 连接里 `exchange_rate = 1`，
`加价%` = 你的毛利率，取整选 `ceil_tenth`。

| 商品 | TUNVN 成本 | 加价 60% | 加价 100% |
| --- | --- | --- | --- |
| MUSE AI 1B TOKEN (NW) | 0.06 USDT | 0.10 | 0.12 |
| Capcut Pro 7D | 0.19 USDT | 0.31 | 0.38 |
| Capcut Pro 1M (FW) | 1.04 USDT | 1.67 | 2.08 |
| 100$ API Claude 30D | 1.50 USDT | 2.40 | 3.00 |
| Grok + Cursor 14D | 4.57 USDT | 7.32 | 9.14 |

> 若站点币种是 CNY，把 `exchange_rate` 设成 USDT→CNY（约 7.1）即可，商品价仍是 USDT 基准。

## 安全与一致性设计（重点）

这套代码里几个关键决定，都是为了防止「重复扣款 / 收钱不发货 / 泄露你的余额」：

1. **幂等**：`downstream_order_no` 是唯一键。TUNVN 的 `/api/buy` **没有任何幂等保护**，
   所以 A 站每次重试都必须命中已存在的订单，绝不能二次采购。已用测试覆盖。
2. **先落盘再花钱**：订单先写入磁盘（原子 rename），再调用 `/api/buy`。
   进程崩溃也留下可追溯记录，不会出现「钱花了但没记录」。
3. **确定性失败同步返回**：余额不足 / 商品下架这类错误在 HTTP 响应里直接返回
   `error_code`，A 站会把采购单标为 `rejected` 并回滚订单，而不是先支付再等回调。
4. **超时按「不可确定」处理**：`/api/buy` 超时可能已在服务端扣款，因此**不重试**，
   订单停在 `paid` + `LastError` 记录，等人工核对；绝不自动补单。
5. **拒绝取消**：TUNVN 不支持退款，已付款订单返回 `cancel_not_allowed`，
   避免 A 站取消了订单而你已经把钱付出去。
6. **不泄露余额**：上游返回的错误文本含你的钱包余额，已过滤后再回给下游。
7. **回调重试**：交付回调失败按 5s/15s/30s/1m/2m/5m/15m 退避，同时
   `GET /orders/:id` 始终可查，A 站自身的 30s 轮询也能兜底。
8. **非循环内网回调**：只允许 http/https，且不跟随重定向。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `TUNVN_API_KEY` | 必填 | Shop Bot API Key |
| `TUNVN_BASE_URL` | `https://tunvnmmo.duckdns.org` | 上游地址 |
| `TUNVN_CURRENCY` | `usdt` | `usdt` 或 `vnd` |
| `ADAPTER_API_KEY` | 必填 | 给 Dujiao 用的 API Key |
| `ADAPTER_API_SECRET` | 必填 | 给 Dujiao 用的签名密钥 |
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `DATA_FILE` | `data/orders.json` | 订单持久化文件 |
| `SITE_NAME` | `TUNVN Adapter` | ping 返回的站点名 |
| `PRODUCTS_TTL_SECONDS` | `60` | 商品列表缓存秒数 |
| `BUY_TIMEOUT_SECONDS` | `90` | 单次采购超时 |
| `DEFAULT_CALLBACK_URL` | 空 | 下游未传 callback_url 时的兜底 |

## 测试

```bash
go test ./...          # 单元 + 集成测试（含 mock 上游的成功交付路径）
go vet ./...
```

端到端联调（会真实调用 TUNVN，`DRY_RUN=1` 时只读）：

```bash
DRY_RUN=1 ADAPTER_URL=http://127.0.0.1:8080 \
ADAPTER_API_KEY=... ADAPTER_API_SECRET=... \
go run ./cmd/mockclient
```

它会依次验证 ping、商品、下单、幂等重放、签名回调、订单详情、取消拒绝。

## 已知限制

- TUNVN 无退款 → 客户售后只能靠**换货**（人工再买一份），无法自动退款。
- TUNVN 无单查 → 采购失败后可追溯性依赖本服务的 `data/orders.json`，请定期备份。
- TUNVN 说明文本可能含第三方链接（Google Doc、官网），导入后需人工清理。
- 所有客户共用你一个 TUNVN 余额，建议监控 `/api/balance` 并设置低水位告警。
- 单价/库存同步依赖 Dujiao 的定时任务（默认 5 分钟），秒杀场景可能超卖，
  但超卖只会体现为采购失败，不会重复扣款。
