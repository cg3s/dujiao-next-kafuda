# Quefa 托管直充接入开发计划

依据：[Quefa API 快照](quefa_api.mhtml) · [Quefa 开发者中心](https://tibo.ink/developers) · [AGENTS.md](../AGENTS.md)

## 1. 首期范围与已定决策

- **购买范围**：仅主站开放，支持会员和游客（按商品 `purchase_type` 决定是否开放游客）。每笔订单限单个 Quefa SKU、数量 `1`，走「立即购买」。购物车加购/混购、分销站、渠道 API 和下游目录一律拒绝。
- **履约模式**：首期仅做 `auto_recharge` + Quefa 托管页（`fulfillment_url`）。买家先完成付款，商城确认到账后在订单详情展示「前往充值」，买家主动点击进入 Quefa 托管页填写并提交 Session。商城不采集、不存储 Session，也不做自建兑换表单（`POST /v1/redemptions` 留作二期）。
- **连接形态**：**单连接全局配置**（参考 `kafudagpt.Setting`，全局唯一 `default` 配置，不接入多连接 `site_connections`）。
- **优惠与钱包底线**：**允许使用钱包余额、优惠券和会员折扣（含促销价/用户专属价），但所有抵扣后的实际付款不得低于成本价。** 具体口径见 §4.3。
- **品牌与域名红线**：**买家侧付款全程不得出现 Quefa 域名或品牌标识。** 从进入收银台、支付链接、二维码解码内容、浏览器网络请求/重定向到回跳商城，只允许出现商城自身或实际支付渠道的域名；买家接口响应的履约类型同步脱敏（不暴露 `quefa` 字样）。付款确认后，仅在买家主动点击「前往充值」时才进入 Quefa 托管页。
- **零库存副作用**：独立履约类型 `quefa_hosted`，不读写、不预占、不回补卡密及人工库存；可售性由全局开关、Worker 就绪态、SKU 定价及有效目录快照共同决定。

## 2. 收款方案与 Q00 联调门禁

| 方案 | 交易流程 | 启用条件 |
| --- | --- | --- |
| 优先验证：Quefa 代收 `platform_collect` | 商城待支付时先持久化本地支付与上游请求，再创建 Quefa 订单。买家经白标收银台或实际支付渠道付款，商城服务端查单确认到账。 | Quefa 提供符合域名红线的自定义收银台域名，或直接返回第三方支付渠道的链接/二维码，且沙箱实测全链路无 `tibo.ink` / Quefa 域名。 |
| 备选：商城收款 `agent_collect` | 买家走商城现有收银台付款。商城确认到账后，由后台队列创建 Quefa 采购单，扣除代理采购余额。 | 代理账号已开通 `agent_collect` 权限、有足额采购余额，且目标 SKU 支持 `auto_recharge` 托管直充。 |

**决策原则**：先完成 **Q00 联调门禁**，根据沙箱实测结果**拍板首期唯一启用的收款模式**，再进入收款/采购路径编码。每笔订单创建时冻结 `collection_mode` 与 `delivery_mode`，后续支付、采购、履约、关单和退款恢复均沿用原单与原模式，不受后台模式切换影响。

### Q00 必须冻结的外部契约

拿到代理工作台的完整接入包、OpenAPI 与沙箱凭据后，选一个实际支持目标收款模式与 `auto_recharge` 的 SKU 完成核验，以下 6 项未确认前不进入对应代码实现：

1. **白标支付与域名链路**：`platform_collect` 下支付链接、二维码原文、302 跳转链、会员/游客回跳参数、支付有效期及应付币种字段。
2. **托管模式下的进度与重提闭环**：通过 `fulfillment_url` 提交后，Quefa 是否推送 `fulfillment.updated` / `fulfillment.succeeded`；订单查询是否返回 `latest_redemption_id`；允许重提（`retry_allowed=true`、`next_action=resubmit`）时是复用原 `fulfillment_url` 还是重新获取新链接；链接过期后的刷新方式。
3. **超时查单与幂等重传边界**：`POST /v1/orders` 网络超时且未拿到 `order_id` 时，按 `merchant_order_no` 查询原订单的接口路径、字段及 `Idempotency-Key` 有效期。
4. **签名与 Webhook 精确定义**：`CANONICAL_QUERY` 编码与排序规则（对照官方 Node.js/Python 示例）、Webhook 请求头名称、时间戳格式与事件正文 Schema，以及支付金额/币种/退款凭证字段。
5. **轮询控制头核实**：确认响应头是否真实提供 `X-Poll-After`、`X-Poll-Stop`，还是仅提供 `X-RateLimit-*` 与 `Retry-After`。
6. **供货价与结算币种**：`GET /v1/products` 返回的供货价、最低售价约束、币种字段及可售状态字段。

参考：[完整接入文档](https://tibo.ink/developers/doc/integration) · [支付通道说明](https://tibo.ink/developers/doc/payment-channels) · [兑换指南](https://tibo.ink/developers/doc/redemption)

## 3. 协议与状态规范

### 3.1 API 签名、Webhook 与错误处理（来自 API 快照）

- **API Base**：固定使用 `https://tibo.ink/v1`（配置化但禁止混用 `api.tibo.ink` 与主站的 POST 签名路径）。
- **请求鉴权头**：每次请求携带 `X-Partner-Id`、`X-Key-Id`、`X-Timestamp`（Unix 秒）、`X-Nonce`（每次请求唯一，重试须重新生成）、`X-Signature`（64 位小写 HMAC-SHA256 十六进制）；写请求必须带 `Idempotency-Key`。Canonical String 固定 8 行：
  `METHOD\nPATH\nCANONICAL_QUERY\nTIMESTAMP\nNONCE\nKEY_ID\nIDEMPOTENCY_KEY\nSHA256_HEX(RAW_BODY)`
- **Webhook 验签与去重**：对原始请求体按 `HMAC_SHA256(webhook_secret, timestamp + "." + RAW_BODY)` 验签；校验时间戳偏差窗口（默认 ±5 分钟，防止重放）；限制请求体大小；先按 `event_id` 唯一索引落库去重，再快速返回 2xx，业务处理交给异步任务并一律回源查单确认终态。
- **已实现事件清单**：`order.paid`、`order.expired`、`order.closed`、`fulfillment.updated`、`fulfillment.succeeded`、`fulfillment.failed`、`fulfillment.cancelled`、`refund.succeeded`、`procurement.refunded`、`webhook.test`（CDK/充值类事件首期忽略但记录去重）。**注意**：退款驳回、核算生成、登记打款无独立 Webhook，仅靠查单或工作台核对。
- **错误与重试矩阵**：
  - 稳定状态看 `status`，业务结果看 `result_code`；`HTTP 202 / queued` 仅代表受理排队，严禁显示“充值成功”。
  - `review / pending` 保持处理中并继续查原任务，严禁统一判失败或触发退款。
  - `409 / idempotency_in_progress`：先查原业务，再用**原 `Idempotency-Key` + 新 `X-Timestamp` + 新 `X-Nonce`** 退避重试，严禁换业务键重下。
  - `429 / 503`：遵循 `Retry-After` 与 `X-RateLimit-Remaining/Reset` 指数退避；写请求保留原 `Idempotency-Key` 与原请求体，重生成时间戳与 Nonce。
  - **网络超时**：绝不直接判失败，先按 `order_id` 或 `merchant_order_no` 查原单；确认上游未建单且协议明确允许时，才用原请求体和原幂等键重传；超过时限仍未知转入后台待核对。
  - **代理无主动取消权限**：上游建单后无法主动调用取消接口，未知结果只能查询原订单/原任务。

### 3.2 三层状态映射

| Quefa 上游状态 / 信号 | 本地履约子状态（`quefa_orders.stage`） | 商城主订单状态 | 买家侧可见行为 |
| --- | --- | --- | --- |
| `pending`（仅 `platform_collect`） | `waiting_payment` | `pending_payment` | 展示白标/支付渠道收银台，支持返回继续付款 |
| 商城已付、待建采购单（仅 `agent_collect`） | `waiting_procurement` | `paid` | 显示「处理中，正在准备充值入口」 |
| `agent_collect` 采购余额不足 | `procurement_blocked` | `fulfilling` | 保留已付事实，买家见「处理中」，后台高亮待处理 |
| `paid` 且尚未提交 Session | `waiting_submit` | `paid` | 展示「待提交 Session」与「前往充值」按钮 |
| `running` / `queued` / `review` | `recharging` | `fulfilling` | 展示公开阶段与更新时间，禁止重提与退款 |
| `failed` / `cancelled` 且 `retry_allowed=true` & `next_action=resubmit` | `resubmit_required` | `fulfilling` | 展示脱敏原因，允许买家重新点击「前往充值」 |
| `failed` / `cancelled`（不可重提）或超时未知 / 金额异常 | `manual_review` | `fulfilling` | 展示「人工处理中」，关闭重提，转客服核对 |
| `recovery_action=refund`（未确认退妥） | `refunding` | `fulfilling` | 立即关闭重提入口，显示「退款处理中」 |
| `succeeded` | `succeeded` | `completed` | 幂等写交付记录、汇总父单、发完成通知、停轮询 |
| `expired` / `closed`（确认未付款） | `closed` | `canceled` | 关闭本地待付单并释放预扣钱包余额 |
| 核实 `refund.succeeded`（`platform_collect`）或商城售后退妥（`agent_collect`） | `refunded` | `refunded` | 记录退款凭证，关闭所有履约与重提入口 |

> 去旧规则：同一订单按 `attempt_no` 递增优先；同一兑换任务按 `progress_version` 递增优先。`succeeded` / `refunded` 终态不可被旧 `running` 或过期事件回退。

## 4. 系统设计与模块落点

### 4.1 模块与插件装配

- 沿用仓库模块化分层与编译期插件 SPI：
  - 新增业务模块 `internal/modules/quefa/{domain,application,infrastructure/gormstore,infrastructure/client,transport/http,contract}`。
  - 新增插件桥接 `internal/plugins/quefa/` 注册到 [directfulfillment/registry.go](../internal/plugin/directfulfillment/registry.go)（自动接入迁移、容器初始化、后台路由与 Casbin 角色策略）。
  - 跨模块编排放在 `internal/workflows/quefa/`（支付确认回源复核、原子交付完结、关单前查单、退款互斥校验），通过各模块 `contract` 与 `internal/bootstrap/` 装配，确保 `internal/architecture/` 全部测试通过。
- `platform_collect` 如启用，新增独立 `PaymentProviderQuefa` 注册进 `paymentProviderRegistry`，并复用现有 `Payment` 与 `PaymentChannel` 体系；不修改现有第三方支付渠道行为。

### 4.2 核心持久化模型

| 表名 | 职责与关键约束 |
| --- | --- |
| `quefa_settings` | 单连接全局配置（主键 `profile='default'`）：`api_base_url`、`partner_id`、`key_id`、`client_secret_encrypted`、`webhook_secret_encrypted`（AES-256-GCM 加密，后台仅返回是否已配置）、`collection_mode`、`sales_enabled`、`allowed_pay_domains`（允许的白标/渠道域名白名单）、`catalog_ttl_seconds`、`poll_interval_seconds`、`version`。 |
| `quefa_catalog_cache` | 全局目录快照：保存最近一次有效的 `GET /v1/products` 解析结果（`product_code`、标题、供货价、币种、支持模式、可售状态）、`synced_at`、`revision`、失败诊断摘要。刷新失败或响应无效不覆盖上次有效快照。 |
| `quefa_orders` | 履约主表（`order_id` 唯一索引指向实际履约子单）：冻结 `product_code`、`sku_id`、`collection_mode`、`delivery_mode`、`cost_floor_amount`、`settlement_currency`、`merchant_order_no`（唯一索引）、`idempotency_key`、`create_request_snapshot`、`upstream_order_id`（非空唯一索引）、`latest_redemption_id`、`attempt_no`、`progress_version`、`stage`、`upstream_status`、`result_code`、`retry_allowed`、`recovery_action`、`fulfillment_url`（仅受控持久化或按需拉取，绝不进公开列表）、`refund_ref`、`procurement_refund_ref`、`next_poll_at`、`exception_code`、`version`。 |
| `quefa_webhook_events` | Webhook 幂等去重表：`event_id` 唯一索引、`event_type`、`upstream_order_id`、`status`、`received_at`、`processed_at`。 |

**商品与 SKU 关联**：复用 `Product`（`fulfillment_type='quefa_hosted'`）与 `ProductSKU`（复用 `price_amount`、`cost_price_amount` 和规格元数据保存 `product_code` 与目录复核修订号）。目录同步只更新上游可售态和参考供货价，**绝不自动覆盖管理员设置的成本价与售价**；目录缺货或超过 `catalog_ttl_seconds` 时暂停新下单，后台仍保留上次有效快照供排查。

### 4.3 定价、优惠与钱包抵扣底线规则

统一定义单个 SKU 的**成本底线**（`cost_floor`）：
$$\text{cost\_floor} = \max(\text{SKU 管理员成本价},\; \text{目录有效供货价})$$
（要求成本价已明确配置且与订单结算币种一致；币种不匹配或缺失有效供货价时禁止开售，不做隐式汇率估算。）

1. **下单与预览校验（优惠券 / 会员折扣 / 促销 / 专属价）**
   - 允许正常叠加商城优惠券、会员等级折扣、促销活动和用户专属价。
   - 计算出订单折后应付总额 `TotalAmount` 后，强制校验 `TotalAmount >= cost_floor`（且 `> 0`）；若低于成本底线，直接拒绝预览与创建订单并返回明确三语提示（如 `error.quefa_below_cost_floor`）。
   - 下单时将 `cost_floor` 与结算币种冻结写入订单快照，后续改价或目录刷新不影响已创建订单。
2. **支付阶段钱包抵扣上限（保证抵扣后实际付款不低于成本价）**
   - 允许会员在支付时勾选使用钱包余额，但**钱包最大可抵扣额封顶为毛利空间**：
     $$\text{max\_wallet\_deduct} = \max(0,\; \text{TotalAmount} - \text{cost\_floor})$$
   - 支付事务中（扩展 [order_wallet_bridge.go](../internal/modules/order/application/order_wallet_bridge.go) 支持抵扣上限）只扣除 `min(用户钱包余额, max_wallet_deduct)`，确保扣除后的**实际在线付款金额** `online_pay_amount = TotalAmount - WalletPaidAmount >= cost_floor`（且严格 `> 0`）。
   - 若 `TotalAmount == cost_floor`，则钱包本次可抵扣 `0`（前端同步明确提示可抵扣上限与剩余需在线支付金额）。
   - 当站点开启 `wallet_only_payment`（仅钱包支付模式）时，因无法发起在线支付以覆盖成本底线，Quefa 商品暂停新下单与新支付。
3. **代收金额与支付手续费口径**
   - `platform_collect` 下：传给 Quefa `POST /v1/orders` 的 `sale_amount` 固定取本地冻结的 `online_pay_amount`（不含额外站内附加项，避免代收通道再叠加 `customer_surcharge` 导致实收与订单对账偏差；Quefa 代收渠道强制校验 `fee_rate == 0 && fixed_fee == 0` 或仅使用商户承担手续费策略）。
   - `agent_collect` 下：沿用商城现有收银台、在线支付手续费及对账口径，支付成功后才排队发起 Quefa 采购。
4. **混合支付的取消与退款拆账**
   - 待支付订单取消或超时关闭时，按现有 `ReleaseWalletBalance` 幂等退回已预扣的钱包余额。
   - 已付混合订单退款时：在线实付部分按所选收款模式处理退款（`platform_collect` 核验 `refund.succeeded`；`agent_collect` 走商城售后退款），钱包抵扣部分独立按 `WalletPaidAmount` 退回用户钱包，严禁把两笔金额混计或重复退回。

### 4.4 白标域名、安全与边界隔离

- **付款域名强校验**：
  - 服务端在保存或返回 `pay_url` / `qr_code` 前，解析 URL（包含二维码内容若为 URL），强制校验 Host **命中 `allowed_pay_domains` 白名单** 且 **绝不匹配黑名单**（`tibo.ink`、`*.tibo.ink`、包含 `quefa` 的默认域名）。校验不通过直接中断并返回安全错误，不向前端下发原始链接。
  - 买家侧边界通过 `constants.BuyerFulfillmentType()` 将 `quefa_hosted` 映射为中性标识（如 `hosted_recharge`）；支付渠道展示名、错误文案、订单详情 JSON 均不得出现 `quefa` 或 `tibo.ink`。
- **托管入口受控下发**：
  - 订单列表与订单详情 GET 接口**不直接返回 `fulfillment_url`**，仅返回安全阶段、`can_open_fulfillment` 布尔位和更新时间。
  - 买家点击「前往充值」时调用独立动作接口：
    - 会员：`POST /api/v1/orders/:order_no/quefa/fulfillment-entry`（校登录态 + 归属 + 主站）
    - 游客：`POST /api/v1/guest/orders/:order_no/quefa/fulfillment-entry`（复用 `ginutil.GetGuestCredentials` 校 `email + order_password` + 主站）
  - 服务端实时校验订单状态允许提交/重提、域名合法且未进入退款后才返回一次性/短期可跳转的 `fulfillment_url`，并施加按订单和 IP 的限流。
- **库存与入口隔离**：
  - 商品类型切换、卡密导入/编辑/导出/恢复、人工库存修改、批量修改库存展示均在商品行锁内拦截 `quefa_hosted`，防止产生库存副作用或破坏已有未完结订单。
  - `cart`、`reseller`、`channelapi`、`upstreamapi` 全部显式拒绝 `quefa_hosted`。
  - 禁止通过普通人工发货（`CreateManual`）或通用订单改状态接口绕过 Quefa 终态。

### 4.5 事务、关单竞态、轮询与售后

- **SQLite 单连接纪律**：配置与开关在事务外预读；外部 HTTP 请求（建单、查单、同步目录）一律在事务外执行；事务内仅使用绑定到当前 `tx` 的仓储，锁定顺序固定为 `parent order -> child order -> quefa_order -> fulfillment`，兼容 SQLite 与 PostgreSQL。
- **读时过期（Lazy Expire）防阻塞**：
  - 现有 [order_service_query.go](../internal/modules/order/application/order_service_query.go) 在读订单列表/详情时会同步取消超时单。
  - `platform_collect` 订单若存在在途上游单号或未确认建单，**读时过期禁止在 GET 请求内同步调 Quefa HTTP，也不直接关单**，而是跳过本地取消并异步入队核查；本地订单 `ExpiresAt` 配置应不短于 Quefa 支付有效期，减少迟到付款。
- **自动关单与手动取消**：
  - 后台定时关单与买家手动取消对代收单统一走核查流：若尚未发过上游请求则直接关单释放钱包；若已发过上游请求，先查原上游单——仅当确认未付款且上游已 `expired/closed` 才关闭本地单。因代理无主动取消权限，上游仍处于可支付状态时拒绝立即取消，提示买家等待支付过期或继续付款。
  - 迟到付款、实付金额或币种与本地快照不符、超时无法确认最终状态时，记录支付/履约异常并转后台 `manual_review`。
- **轮询与原子完结**：
  - 复用现有 Redis Worker + 周期补偿扫描（扫描 `next_poll_at <= now` 的在途单，防止 Webhook 丢失或 Worker 重启丢单）。开售前校验 Worker 与定时补偿就绪；停售后在途订单继续同步直至终态。
  - 查单确认 `succeeded` 后，在单事务内复核订单未退款/未完结、幂等创建脱敏 `Fulfillment`（`type='quefa_hosted'`）、将子单置为 `completed` 并汇总父单；事务提交后触发邮件/站内/Bot 通知，重放只交付一次。
- **退款隔离**：
  - Quefa 返回 `recovery_action=refund` 时立即关闭重提入口。
  - `platform_collect`：仅在回源查单核实 `refund.succeeded` 后，按上游退款凭证去重登记退款记录、更新订单状态、退回混合支付的钱包部分并回滚分销佣金。
  - `agent_collect`：客服先在后台主动查单确认上游不会再交付，再经现有商城售后流程处理买家退款（会员可退原路/钱包，游客仅支持线下登记 `manual` 退款）。`procurement.refunded` 仅写入 `procurement_refund_ref` 供采购对账，绝不驱动买家订单变为已退款。

### 4.6 路由、RBAC 与前后台落点

- **后台接口与 RBAC（通过 `directfulfillment.Plugin.RBACRules()` 自动注册）**：
  - `integration`：`GET/PUT /admin/quefa/config`、`POST /admin/quefa/config/test`、`GET /admin/quefa/catalog`、`POST /admin/quefa/catalog/sync`
  - `operations`：`GET /admin/quefa/catalog`、`POST /admin/quefa/catalog/sync`（供商品编辑页绑定 SKU）
  - `support`：`GET /admin/orders/:id/quefa`、`POST /admin/orders/:id/quefa/sync`（主动查单）、`POST /admin/orders/:id/quefa/mark-review`（人工核对备注/闭环）
  - `finance`：沿用现有支付渠道配置接口（`platform_collect` 渠道参数与域名白名单校验）
- **Webhook 路由**：挂载在 `/api/v1/webhooks/quefa`（位于 `/api` 保留前缀内，不触发 SPA 回退）。
- **前端范围（三语 `zh-CN` / `zh-TW` / `en-US` 同步补齐）**：
  - 后台：新增 Quefa 全局配置页（凭据配置状态、模式、域名白名单、测试连接、同步目录）；扩展 `ProductEditModal.vue` 支持 `quefa_hosted` 类型、按 `product_code` 绑定 SKU 与定价；订单详情增加 Quefa 状态卡片、上游单号/异常原因与「主动查单」按钮。
  - 前台（同时覆盖 `classic` 与 `vault`）：商品详情与结算页展示抵扣后实付提示及钱包可抵扣上限；会员订单详情（`OrderDetail.vue`）与游客订单详情（`GuestOrderDetail.vue`）展示「待提交 Session / 充值中 / 需重新提交 / 退款处理中 / 已完成」状态，并在允许时提供「前往充值」按钮。

## 5. 分阶段开发计划

| 阶段 | 依赖 | 目标与关键交付 | 完成标准 |
| --- | --- | --- | --- |
| **Q00 契约冻结与沙箱选型** | 接入包 / 沙箱凭据 | 核实 §2 的 6 项外部契约，实测 `platform_collect` 域名链与托管页提交流程，拍板首期唯一收款模式。 | 产出一页确认结论，锁定收款模式、字段映射与白标域名方案。 |
| **Q01 模块骨架与持久化基础** | Q00 | 新建 `internal/modules/quefa` 与 `internal/plugins/quefa`；实现 HMAC 请求签名、测试连接、凭据加密配置、4 张核心表迁移及架构分层测试。 | `go test ./internal/architecture/...` 通过；SQLite/PG 迁移可重跑；真实/模拟客户端签名通过官方测试向量。 |
| **Q02 目录同步与后台商品绑定** | Q01 | 实现 `GET /v1/products` 同步与 TTL 缓存；后台全局配置页及 `ProductEditModal` 接入 `quefa_hosted`；补齐类型切换锁互斥与零库存副作用保护。 | 目录刷新失败保留旧快照、不覆盖成本/售价；卡密/人工库存全部隔离。 |
| **Q03 购买校验、定价底线与入口隔离** | Q02 | 下单与预览校验主站、单 SKU、数量 1、可售态与 `TotalAmount >= cost_floor`；封堵购物车/分销/渠道 API/下游目录；冻结订单快照与买家类型脱敏。 | 折后低于成本价被拒；所有非主站入口拦截；买家接口不暴露 `quefa` 与成本价。 |
| **Q04 收款建单与钱包封顶** | Q03 | 落地已选定的收款模式（`platform_collect` 外部收银台 + 域名校验，或 `agent_collect` 支付后排队采购）；实现钱包最大可抵扣额封顶与幂等建单/超时查单恢复。 | 钱包抵扣后实付永不低于 `cost_floor`；并发点击/超时/重启复用原单；`wallet_only_payment` 正确暂停销售。 |
| **Q05 托管入口、Webhook 与原子交付** | Q04 | 实现会员/游客按需获取 `fulfillment_url`；实现 Webhook 验签+`event_id` 去重、Worker 轮询退避与补偿扫描；按 `attempt_no`/`progress_version` 去旧；`succeeded` 原子完结并通知。 | 跨账号/错游客凭据被拒；乱序/重放 Webhook 不回退终态；成功交付与通知仅执行一次。 |
| **Q06 关单竞态、退款与后台核对** | Q05 | 改造读时过期异步化与自动/手动关单前查单；实现退款闭环（区分 `refund.succeeded` 与 `procurement.refunded`、混合支付钱包拆退、游客退款）；补齐后台核对与 RBAC 覆盖测试。 | 关单不误杀已付单、不阻塞列表接口；`rbac_coverage_test.go` 与通用改状态防绕过测试通过。 |
| **Q07 前台 Classic & Vault 与三语** | Q04–Q06 | 适配两套店面的商品/结算钱包上限展示、会员与游客订单详情状态轮询及「前往充值」入口；补齐前后台与 API 三语文案。 | 两套店面构建、单测及会员/游客端到端流程通过，无硬编码文案。 |
| **Q08 全量回归与试点上线** | Q01–Q07 | 执行付款域名审计、双库（SQLite 单连接 + PostgreSQL）+ Redis Worker 重启回归、既有功能回归（Kafuda/普通支付/卡密/上游采购），准备单 SKU 灰度。 | 所有 `AGENTS.md` 必跑命令与 §6 验收项全绿。 |

## 6. 验收矩阵与灰度上线

| 验收维度 | 必须通过的条件 |
| --- | --- |
| **付款域名与白标隔离** | 抓包检查前端响应、支付链接、二维码解码原文、302 跳转与回跳页，付款全程无 `tibo.ink` / `quefa` 域名与字样；覆盖会员、游客、返回待付订单续付和支付超时。 |
| **定价与钱包抵扣底线** | 优惠券 + 会员折扣后低于 `cost_floor` 拒绝下单；高于 `cost_floor` 时钱包仅抵扣差额、在线实付 `>= cost_floor`；待付取消退回钱包；已付退款正确拆分在线与钱包部分。 |
| **托管提交与权限安全** | 会员与游客付款后可经授权接口打开原单托管页；覆盖关页恢复、链接失效、允许重提、完成后收到旧进度、以及跨账号或错误游客凭据拦截。 |
| **幂等与故障恢复** | 并发下单、上游已建单但响应超时、Webhook 早于创建响应、Webhook 重放/乱序、409/429 限流、Redis Worker 重启均能恢复原单且仅交付一次；停售后在途单继续完结。 |
| **关单与资金售后** | 读时过期不发同步 HTTP；迟到付款、金额/币种不符进入人工核对；`recovery_action=refund` 立即关重提；`procurement.refunded` 不触发买家退款。 |
| **仓库级回归** | SQLite 单连接 + PostgreSQL + Redis 回归通过；按 [AGENTS.md](../AGENTS.md) 通过 `gofmt`、`go vet ./...`、`go test ./...`、两端前端 `pnpm run test && pnpm run build`；回归 Kafuda、普通支付、卡密、既有上游采购、Classic & Vault、三语和后台 RBAC。 |

**灰度策略**：默认 `sales_enabled=false`。完成 Q00 沙箱验收与 Q08 回归后，先开放 **1 个 SKU** 小流量试点，核对至少一笔真实付款、托管 Session 提交、自动完结及一笔退款/对账记录无误后，再逐步放开其余 SKU。
