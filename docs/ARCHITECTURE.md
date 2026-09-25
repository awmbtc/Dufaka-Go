# Dufaka-Go 架构

Dufaka-Go 按原发卡站 2.0.6（`assimon/dujiaoka`，提交 `e846d12`）的顾客页面和店主功能，用 Go 与 PostgreSQL 重新实现。页面样式沿用原站的样式文件。仓库名 Dufaka-Go。

当前验收口径（2026-09-25）：前台三套皮肤按原站页面布局和外观核对，保留 Dufaka-Go 品牌。后台保留现有 macOS 27 风格，可修复布局、显示和功能缺失，不对标原站后台外观。本文以代码为准；`AUDIT-2026-09-23.md`、`AUDIT-2026-09-25.md` 记录审计发现与修复。

## 进程

一个 Go 进程同时提供前台、后台、支付通知和定时任务。数据只放 PostgreSQL。不安装 Redis，不另开队列进程。

进程只听本机，nginx 对外提供 HTTPS。安装完成后，安装地址关闭。

```
浏览器 → nginx → Dufaka-Go → PostgreSQL
                 ├── 前台三套模板
                 ├── 后台 /admin
                 ├── 支付通知
                 └── 过期与 cldx 轮询
```

启动配置只有监听地址、数据库连接和会话密钥（`dufaka.json` 或环境变量 `DUFAKA_ADDR`、`DUFAKA_DATABASE_URL`、`DUFAKA_SESSION_KEY`、`DUFAKA_BASE_URL`；cldx 钱包用 `WALLET_MERCHANT_SECRET` 等）。站名、模板、邮件和推送都在设置表里，由原来的「系统设置」页修改。

启动顺序：打开连接池后先 `Ping`（5 秒超时）。连不上时把「请检查 DUFAKA_DATABASE_URL」写进日志，下面所有启动步骤都跳过（日志写明跳过），请求侧仍按未安装处理（进入安装页），数据库恢复后自动接上。连得上时依次执行，每一步单独限时 10 秒，超时或出错只记日志、不阻塞启动：

1. `Installed`：判断是否已安装；未安装则后面几步都不做。
2. `EnsureCldxSchema`：只要站点已安装就执行，与是否配置钱包无关。补齐 `orders` 的 cldx 三列和两个轮询部分索引，删除旧索引 `idx_orders_cldx_wait`；已经齐全的步骤直接跳过，不锁表；不依赖 `pays` 表上的任何约束。
3. `EnsureCldxPay`：只在配了 `WALLET_MERCHANT_SECRET` 时执行，用 `INSERT … WHERE NOT EXISTS` 补写 cldx 渠道行（不用 `ON CONFLICT`，老库 `pay_check` 没有唯一约束也能执行）。
4. `DisableUnwiredPays`：把所有启用了但本版本没有收银台的支付渠道置为停用，并记录停用了几行。
5. `DisableCaptchaSwitches`：把 `is_open_geetest`、`is_open_img_code` 中不为 `0` 的行改成 `0`，记录改了几行，并使站点设置缓存失效。

缺 `DUFAKA_SESSION_KEY` 的已安装站点直接退出。

站点设置（`store.Site`）在进程内缓存 10 秒；后台保存系统设置和 `PutSetting` 都会立即使缓存失效。失效带一个代数计数：正在读取设置表的请求如果在读完之前被人失效，读到的值不会写回缓存，所以后台保存不会被一次慢读盖掉。

后台任务每 30 秒一轮（一轮最长 25 秒，上一轮没结束就跳过并记日志）：先把超过 `order_expire_time` 分钟的待支付订单置为过期，再轮询 cldx 钱包。读不到系统设置时（`SiteOK` 返回失败）本轮跳过过期清理并记日志，不拿内置的 5 分钟默认值去过期订单；cldx 轮询照常。

## 目录

```
cmd/dufaka/            进程入口、启动检查、后台任务循环
internal/httpx/        前台路由、安全头、静态文件、cldx 收银与轮询；templates/ 内嵌三套皮肤模板
internal/store/        数据访问：下单、结算、发货、过期、支付渠道、站点设置缓存
internal/admin/        后台
internal/order/        计价（分为单位）、优惠码规则
internal/pay/          支付渠道驱动（微信 APIv3 Native、cldx 钱包）
internal/netx/         客户端地址规则（`netx.ClientIP`）
internal/install/      安装向导，内嵌 schema.sql
internal/testdb/       测试用一次性 schema
web/assets/            前台静态文件（`/assets/`）
sql/001_schema.sql     全量建表
sql/002_cldx_quote.sql 已有站点：cldx 报价与截止列
sql/003_cldx_poll.sql  已有站点：cldx 轮询轮转列与两个部分索引（CONCURRENTLY，须在事务外执行）
.github/workflows/test.yml  CI：vet、-race 测试（PostgreSQL 17 服务）、govulncheck
```

三套前台皮肤各自保留，模板以 `internal/httpx/templates` 内嵌进二进制。系统设置里切换模板后，下一页使用对应皮肤。后台按原站 Dcat 的菜单、列表、表单和按钮重做，不换成另一套管理界面。

静态文件只从 `web/assets` 提供：目录（以 `/` 结尾或解析为目录的路径）一律 404，任何以 `.` 开头的路径段（`.git`、`.env`、`._*`）也 404；`index.html` 不是可服务的静态文件名。`internal/httpx` 和内嵌模板不在任何可访问路径上。所有响应带安全头：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: strict-origin-when-cross-origin`、`Content-Security-Policy: frame-ancestors 'none'`；请求经 HTTPS（直连 TLS 或代理带 `X-Forwarded-Proto: https`）时另加 `Strict-Transport-Security`。后台的非 GET 请求校验 `Origin` / `Sec-Fetch-Site`，跨站一律 403。

## 前台网址

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/` | 首页 |
| GET | `/buy/{id}` | 商品 |
| POST | `/create-order` | 下单，成功后进入结算页 |
| GET | `/bill/{orderSN}` | 结算 |
| GET | `/detail-order-sn/{orderSN}` | 订单详情 |
| GET | `/order-search` | 查询页 |
| GET | `/check-order-status/{orderSN}` | 轮询。未支付 `400000`，已支付 `200`，过期或不存在 `400001` |
| POST | `/search-order-by-sn` | 按订单号 |
| POST | `/search-order-by-email` | 按邮箱。开启查询密码时必须提交密码 |
| POST | `/search-order-by-browser` | 按本机 cookie |
| GET | `/install` | 未安装时可打开 |
| POST | `/install/test` | 安装页的连接测试 |
| POST | `/do-install` | 执行安装 |
| GET | `/pay-gateway/{handle}/{payway}/{orderSN}` | 转入具体支付 |
| GET | `/pay/{channel}/{payway}/{orderSN}` | 同上，原站路径 |
| POST | `/pay/wepay/notify_url` | 微信支付通知 |
| GET | `/cldx/pay?order=订单号` | cldx 二维码落点：只接受订单号，从库里锁定的报价重建钱包参数后 302 到 `clodex://pay?...`，App 必须跟随这个 302。订单未知、未报价、已过期或不是 cldx 渠道一律 404 |

没有 `/check-geetest`，也没有 `/cldx/{id}`：二维码里只有订单号，收款方、金额和截止时间永远来自小店自己的配置和已锁定的报价，链接被改也改不了收款方。

下单字段名不变：`gid`、`email`、`payway`、`by_amount`、`search_pwd`、`coupon_code`，以及人工商品的额外输入框。图形验证码和极验两个开关在本版本不可用：系统设置页显示为关闭且不可勾选，保存时一律写成 `0`，已安装站点每次启动也会把库里残留的 `1` 清成 `0`（`DisableCaptchaSwitches`）；下单（`store.CreateOrder`）完全不看这两个开关，`img_verify_code` 与极验字段被忽略。`Site.GeeTest` 只用于显示。

下单成功后写入 cookie `dujiaoka_orders`，值是订单号数组。浏览器查询读这个 cookie。

每个皮肤都包含首页、购买、结算、订单详情、查询、二维码支付和错误页。文案有简体和繁体。站名、logo、关键词、公告、页脚、默认模板和 Google 翻译开关都来自系统设置。

下单校验与原站相同：

- 自动发货商品的库存显示未售卡密数量。人工处理商品用库存数字。
- 单次限购大于 0 时，超过即拒绝。
- 购买数量是大于 0 的整数。库存不足即拒绝。
- 邮箱必填，并且必须是邮箱格式。
- 开启查询密码时，密码不能为空。新安装的站点默认开启查询密码（`is_open_search_pwd=1`）。
- 支付方式必须存在、启用，而且本版本有收银台（`store.CashierChecks`：`wescan`、`cldx`），否则「支付方式不可用」。
- 商品不存在或未启用即拒绝。
- 该商品有循环卡密时，数量只能是 1。
- 优惠码必须属于该商品、处于启用、剩余次数大于 0。
- 人工商品按配置检查额外输入框。内容写入订单详情，一行一条，形式为「说明:值」。
- 订单记录的 `buy_ip` 由 `netx.ClientIP` 决定：只有 TCP 对端是受信任的代理时才读代理头，只读 `DUFAKA_REAL_IP_HEADER` 指定的一个头（默认 `X-Real-IP`；设为 `X-Forwarded-For` 时只取最后一段），值必须能解析为 IP；其他情况一律用对端地址。受信任代理默认只有本机回环地址（nginx 与本进程同机）；代理在别的机器上时用环境变量 `DUFAKA_TRUSTED_PROXIES` 指定（写法见 `internal/netx`）。登录与下单的按 IP 限速用 `netx.LimiterKey`：IPv4 按单个地址，IPv6 按 /64 网段。

按邮箱查询：开启查询密码时必须提交密码，命中的订单完整显示。查询密码关闭时，邮箱本身不能当凭证：只列出订单，订单号打码显示，卡密内容不给；完整内容仍可凭订单号打开，或由下单的浏览器（cookie）查看。

价格只在服务器计算（`internal/order`，以分为单位）：

- 总价 = 实际售价 × 数量，保留两位小数。
- 批发按数量阶梯取单价。优惠额 = 原总价 − 批发总价。
- 优惠码是固定减免。
- 实付 = 总价 − 优惠码 − 批发优惠。结果小于等于 0 时拒绝下单（「实付金额必须大于 0」）。

订单号是 16 位大写字母和数字。状态值不变：

| 值 | 含义 |
| --- | --- |
| 1 | 待支付 |
| 2 | 待处理 |
| 3 | 处理中 |
| 4 | 已完成 |
| 5 | 处理失败 |
| 6 | 异常 |
| -1 | 过期 |

前台报错：`store.RuleError` 是给顾客看的业务提示（如「订单不存在」「库存不足」），原样显示。前台用到的查询（`OrderBySN`、`OrdersByEmail`、`CreateOrder`、`Home`）遇到其他失败（数据库连不上、SQL 报错、请求取消）返回 `store.InternalError`：`Error()` 只有「系统繁忙，请稍后再试」，原始错误在 store 里记一次日志，并可用 `errors.Is`/`errors.As`/`Detail()` 取到，不会把连接串、SQLSTATE 或回显的输入印到公开页面上。无效 UTF-8、含 NUL 或超过 64 字节的订单号直接当「订单不存在」，不送进数据库。

## 后台

默认入口是 `/admin`。登录后的菜单与原站相同：

- 控制台：销售额、成功订单、支付占比
- 商品、分类
- 卡密，含批量导入和循环开关
- 优惠券
- 邮件模板
- 支付渠道
- 订单，查询密码可复制
- 系统设置
- 邮件测试
- 管理员、角色、权限、菜单

列表、筛选、新增、编辑、删除和恢复软删除都保留。支付渠道表单含名称、标识、跳转或扫码、电脑或手机或全部、商户号、商户 KEY、商户密钥、处理路由、启用。

系统设置仍是四个标签：基本设置、订单推送、邮件、极验。字段名不变，例如 `title`、`template`、`order_expire_time`、`is_open_search_pwd`、`is_open_geetest`、`driver`、`host`。其中 `is_open_img_code` 与 `is_open_geetest` 在本版本固定存为 `0`，页面上不可打开，启动时也会清成 `0`，下单不看它们。保存后前台的站点设置缓存立即失效。

订单详情页对「异常」（状态 6）的自动发卡订单提供「重新发货」：店主补卡后再走一次结算路径。卡仍不够时返回「库存不足」，订单原样不动（详情、交易号都不改，也不会写入占位交易号 `manual`）；补发成功时交易号为空的订单才记为 `manual`。人工处理商品不提供重新发货（「仅自动发卡商品支持重新发货」），由店主按订单人工处理。cldx 的状态 6 订单不会再被轮询（轮询只看 1 和 -1），补卡后必须由店主在后台点「重新发货」。

支付渠道页只允许启用 `wescan` 与 `cldx`；其他渠道的行可以看、可以编辑，但不能启用，进程启动时也会把它们统一停用。

## 支付

本版本只有两个渠道有收银台，其他都没有：

| 标识 | 名称 | 路径 | 状态 |
| --- | --- | --- | --- |
| wescan | 微信扫码 | `/pay/wepay` | 有收银台（APIv3 Native） |
| cldx | cldx 钱包 | `/pay/cldx` | 有收银台（钱包报价 + 轮询到账） |
| 其他（支付宝、易支付、PayPal、Stripe、TokenPay 等原站渠道） | — | — | 没有收银台 |

有收银台的渠道列表只有一处：`store.CashierChecks`。结算页（`store.Pays`）只列出已启用、场景匹配电脑/手机/全部、且在该列表里的渠道；下单时再校验一次（`CreateOrder`）；后台不允许把列表外的渠道启用；进程启动时 `DisableUnwiredPays` 把库里启用着的其他渠道全部停用并记日志。所以一个没有收银台的渠道即使被人直接改库启用，也到不了顾客面前。

到账金额必须和订单实付一致，精确到分，才发货。金额不一致不修改订单。

微信扫码仍显示二维码。底下使用现有商户的微信支付 APIv3 Native。原站源码是 APIv2，现有商户不能用那套密钥付款，所以只换协议，不换页面，也不做 H5 自动拉起。

微信配置以后台 `wescan` 渠道行为准（商户 ID、商户密钥、商户 KEY），其余证书级字段读环境变量 `WECHAT_PAY_APP_ID`、`WECHAT_PAY_CERT_SERIAL_NO`、`WECHAT_PAY_API_V3_KEY`（商户 KEY 留空时）、`WECHAT_PAY_PLATFORM_KEY`、`WECHAT_PAY_PLATFORM_SERIAL`、`WECHAT_PAY_PUBLIC_KEY_ID`。旧部署用的 `WECHAT_PAY_MCH_ID`、`WECHAT_PAY_PRIVATE_KEY` 已弃用，但渠道行对应字段为空时仍会读取作为兜底；请改填到后台渠道行。配置是否齐全由 `pay.WechatConfig.Validate()` 判定（返回空表示可用）。

订单已经完成后，如果再次收到金额一致的通知，按该渠道要求回答成功。第一次通知才发货。已付款但库存不足的订单（结算返回 `store.IsPaidShort`，订单已记为状态 6、交易号已写入）通知接口同样回答成功，不让网关反复重试；这类订单由店主补卡后「重新发货」。微信对状态 6 的自动发卡订单再次通知时，会再走一次结算发货：此时库存已补上就直接发货，仍不够则保持状态 6。

### cldx

顾客选 cldx 后，小店向钱包询价把实付（分）折成 cldx 最小单位，第一次成功的报价和截止时间锁进订单（`cldx_minor`、`cldx_expires_at`），之后并发请求都以它为准。收银页显示二维码，二维码只含 `/cldx/pay?order=订单号`；App 打开后跟随 302 得到 `clodex://pay?to=商户&amount=报价&order=订单号&exp=截止`。已装 App 的浏览器（`clodex_app=1` cookie）直接给深链接。

到账以钱包的合同 `(to, order)` 为对账键：小店向钱包查「付给商户 `to` 的订单 `order`」，钱包返回付款方、金额和退款状态，不返回付款编号。到账金额必须等于锁定报价，退款过的不算。发货时写入 `trade_no = "cldx:<order>:<from>"`（订单号加付款方地址）。

轮询：收银页每几秒查一次，后台任务每 30 秒扫一次（`SyncCldx`）。同一订单的钱包回答在进程内缓存 10 秒，收银页和后台共用，多开几个标签也只打一次钱包。

后台一轮最多取 200 个订单（`store.WaitingSNs`），分两个桶分别读取再 `UNION ALL`：待支付（状态 1）的桶保底 160 个，过期不超过 24 小时（`CldxLateWindow`，按 `cldx_expires_at`）的迟到单桶保底 40 个；某个桶不够自己的份额时，剩下的名额让给另一个桶，合计不超过 200。返回的列表按份额交错排列（每 4 个待支付单后跟 1 个迟到单），而不是把迟到单排在末尾：同步按顺序逐单查询、到 25 秒截止就停，所以哪怕一轮只查完开头一小段，其中也约有五分之一是迟到单，查过的会写回 `cldx_polled_at` 排到桶尾。钱包变慢或待支付订单积压只会让迟到单轮得慢一些，不会一直轮不到。每个桶内按 `cldx_polled_at` 最久没问过的优先（没问过的排最前，再按 id），每问一次写回 `cldx_polled_at`，所以订单再多也会轮到。两个桶各有一个部分索引：`idx_orders_cldx_live (cldx_polled_at NULLS FIRST, id) WHERE status=1 AND cldx_minor IS NOT NULL AND deleted_at IS NULL` 和 `idx_orders_cldx_late (cldx_expires_at) WHERE status=-1 AND cldx_minor IS NOT NULL AND deleted_at IS NULL`。一轮里最多 8 路并发向钱包查询，整轮共用 25 秒的截止时间，没轮到的留到下一轮。

过期超过 24 小时的订单后台不再轮询，留给人工对账；但顾客仍开着收银页（`/check-order-status`）时，收银页的轮询对状态 1 和 -1 的订单不看时间窗，仍会按需查一次钱包（同样受 10 秒缓存约束），到账就照常结算。状态 6 的 cldx 订单两种轮询都不再查，见后台「重新发货」。

## 发货和通知

确认付款与发货在同一个数据库事务中完成。

下单时就为自动发货商品预留卡密（`carmis.reserved_order_id`），人工商品则直接扣库存；过期时预留和库存都退回（只退本单商品的预留，`goods_id` 限定）。所有事务按同一把锁序取行锁：`goods` 行（`FOR NO KEY UPDATE`）→ `carmis` → `coupons`（结算和过期先锁 `orders` 行，再锁 `goods`；它们在 `goods` 之后先改优惠码再动卡密，因为同一商品的卡密已被 `goods` 行锁串行化，而优惠码是唯一跨商品共享、且每个事务最多锁一行的行，这个先后不会成环）。因此迟到付款、过期清理和并发下单不会互相死锁。

锁 `goods` 行用 `FOR NO KEY UPDATE` 而不是 `FOR UPDATE`：它照样与其他下单、结算、过期互斥，但不挡外键插入要取的 `FOR KEY SHARE`。后台保存优惠码是「先 `UPDATE coupons`、再 `INSERT coupons_goods`」，插入要对所指商品行取 `FOR KEY SHARE`；用 `FOR UPDATE` 时，它和同一优惠码的迟到付款或过期会互相等待而死锁（40P01）。下单锁优惠码行也用 `FOR NO KEY UPDATE`。

下单事务里的所有读取（商品、卡密、支付渠道、优惠码、写入后的订单）都走同一个事务连接，不在持有事务时再向连接池要第二条连接；否则连接池较小时，并发下单会把池子占满后互相等待。

结算（`store.Complete` / `Redeliver` 共用 `settle`）：

- 自动发货时锁定本单预留的卡密，不够再补未预留的（`FOR UPDATE SKIP LOCKED`）。循环卡密不改为已售。普通卡密改为已售，原文写入订单详情，状态改为已完成。数量不够时，订单改为异常（6），详情写「库存不足」（自动发货订单没有买家输入，详情整段替换），交易号照写并提交，不把卡密标成已售；店主补卡后可在后台「重新发货」。
- 人工处理时状态改为待处理。库存在下单时已扣，只有过期（-1）后迟到付款才重新扣一次；这时库存不够，同样记为异常（6）、写交易号并提交，让付了款的订单能被店主看见，而不是一直留在过期状态被轮询一天。人工订单的详情里是买家填的额外输入，不能丢：详情改为「库存不足」加换行再接原来的内容（`'库存不足' || E'\n' || COALESCE(info,'')`）。异常的人工订单再收到通知只确认不再动库存，也不能重新发货。
- 两种成功都会增加销量。原站在这之后按开关发送 Server 酱、Telegram、Bark 和企业微信，并向商品回调地址以 JSON POST `title`、`order_sn`、`email`、`actual_price`、`order_info`、`good_id`、`gd_name`；本版本这些都还没有接通（见下）。

待支付超过 `order_expire_time` 分钟后改为过期，并退回本单占用的优惠券次数，写入 `coupon_ret_back=1`。过期、退券、释放预留卡密和退回人工商品库存在同一个事务里完成。

过期后迟到付款：`coupon_ret_back=1` 的订单在结算时把优惠码次数再扣回去（只扣未删除且还有次数的优惠码），成功则 `coupon_ret_back=0`。扣不回（次数已被别人用完或优惠码已删）仍然发货——顾客已经按折后价付了钱——并写 `coupon_ret_back=2`（已退回且无法再扣）加一条日志，供店主人工核对；之后的重复通知和重新发货看到 2 就不再重试扣码。

邮件模板标识保持为 `card_send_user_email`、`manual_send_manage_mail`、`pending_order`、`completed_order`、`failed_order`。占位符保持为 `{webname}`、`{weburl}`、`{ord_title}`、`{ord_info}`、`{order_id}`、`{buy_amount}`、`{ord_price}`、`{created_at}`、`{product_name}`。正文重新编写，顾客看到的信息与原站一致。

当前版本还没有任何代码发信：`card_send_user_email`、`manual_send_manage_mail` 等只是安装时种下、可在后台编辑的模板，发货和人工订单都不会真的发邮件；Server 酱、Telegram、Bark、企业微信推送与商品回调同样只有设置项，尚未接通（`jobs` 表也还没有写入方）。

## 表

金额使用 `numeric(10,2)`。状态使用原来的整数。软删除保留 `deleted_at`。

业务表是 `goods_group`、`goods`、`carmis`、`coupons`、`coupons_goods`、`orders`、`pays`、`emailtpls`。

后台表是 `admin_users`、`admin_roles`、`admin_permissions`、`admin_menu`，以及原站的角色关联表。管理员密码使用 bcrypt。

`settings` 保存系统设置，键与原表单相同。后台仍是原来的系统设置页。配置不再只放在缓存里，因此清缓存不会丢掉站名、模板和邮件参数。

`orders` 另有 cldx 三列：`cldx_minor`（锁定报价）、`cldx_expires_at`（截止）、`cldx_polled_at`（上次轮询时间），以及两个部分索引 `idx_orders_cldx_live`、`idx_orders_cldx_late`（见 cldx 一节；取代旧的 `idx_orders_cldx_wait`）。已有站点按顺序执行 `sql/002_cldx_quote.sql`、`sql/003_cldx_poll.sql`；003 用 `CREATE INDEX CONCURRENTLY IF NOT EXISTS` 和 `DROP INDEX … IF EXISTS`，必须在事务外执行（例如 `psql -f`，不要加 `--single-transaction`），建索引时不挡下单写入。新安装的 `001_schema.sql` 已包含（普通 `CREATE INDEX IF NOT EXISTS`）。进程启动时 `EnsureCldxSchema` 也会补建缺的列和索引，但用的是普通 `CREATE INDEX`，建索引期间会挡住 `orders` 的写入，所以订单多的站点应先手动执行 003。

`jobs` 表为待发送的邮件、推送和回调预留（含失败原因与重试时间），当前版本没有代码写入或消费它。前台没有这个页面。

领取卡密使用行锁：`SELECT … FOR UPDATE SKIP LOCKED`。两人同时买最后一张时，一人成功，另一人得到原来的库存不足异常。普通购买看不出差别。所有 SQL 都是参数化语句，页面用 `html/template` 输出。

查询密码和卡密按原站以明文存在订单中。后台可以查看和复制。

## 安装

未安装时，除安装页和静态文件外都进入安装页。安装写入管理员和站点地址。完成后 `/install` 不再接受提交。没有免登录的连接测试。

初始支付渠道只种 `wescan`（默认停用，填好商户资料后启用），cldx 由启动时按钱包密钥写入；邮件模板与原站种子数据一致。新安装默认开启查询密码。

## AI

默认关闭。关闭时前台和后台与原站一致，不出现新按钮。打开后只增加助手，不改购买、计价、锁卡和支付。

调用放在服务器。浏览器拿不到密钥。协议用 OpenAI 兼容的 Responses 接口。默认接 SpaceXAI：`https://api.x.ai/v1`，模型 `grok-4.7`。密钥用环境变量 `XAI_API_KEY`，也可在系统设置里改地址、模型和密钥。GoToAI 同样是这套协议，官方店若要走自己的中转，只改地址和密钥。

设置项放在系统设置的新标签「AI」里，默认全关：

| 键 | 作用 |
| --- | --- |
| `ai_enabled` | 总开关，默认关 |
| `ai_base_url` | 默认 `https://api.x.ai/v1` |
| `ai_model` | 默认 `grok-4.7` |
| `ai_api_key` | 留空则读环境变量 |
| `ai_product_copy` | 商品文案助手 |
| `ai_mail_draft` | 邮件模板草稿 |
| `ai_notice_draft` | 公告和页脚草稿 |
| `ai_order_hint` | 后台订单风险提示 |
| `ai_buyer_help` | 前台购买说明 |

适合接入的地方：

- 商品编辑页：根据店主写的一两句话，草拟名称、简介、关键词、购买提示和详情。填进现有输入框，店主保存后才生效。不改价格和库存。
- 邮件模板页：草拟标题和正文，必须保留原来的占位符。店主保存后才发给顾客。
- 系统设置：草拟公告、页脚和关键词。仍由原字段展示。
- 订单列表：给店主一行风险说明，例如短时间同一邮箱大量下单、额外输入框异常。不自动取消、不自动退款。
- 前台购买页：仅在 `ai_buyer_help` 打开时，商品说明旁边多一个「问购买说明」。它只根据已经公开的商品名、详情和购买提示回答，不查卡密，不创建订单。

明确不交给模型的数据：卡密原文、订单详情里的卡密、查询密码、支付商户密钥、管理员密码。卡密导入只做本地的分行和去重，不把卡密发到模型。

模型不参与这些决定：实付金额、库存扣减、订单状态、支付是否到账、发哪一张卡。这些仍按前面的事务规则执行。

## 对照

上线前逐页对照 unicorn、luna、hyper 和后台每个菜单。同一输入必须得到同一结果：价格、拒绝原因、订单状态、卡密是否售出、优惠券剩余次数、邮件占位符、支付二维码和三种查询方式。

内部改动只纠正原来会超卖、重复通知被当成失败、配置只存在缓存里，以及审计列出的安全与一致性问题。成功路径上的页面和功能不改变。

CI（`.github/workflows/test.yml`）在 ubuntu 上起 PostgreSQL 17（用户 `shop_test`，trust 认证），依次跑 `gofmt`、`go vet ./...`、`go test -race -count=1 -timeout 10m ./...`（`DUFAKA_TEST_DATABASE_URL=postgresql://shop_test@127.0.0.1:5432/postgres`，每个测试用自己的一次性 schema；没有这个变量时数据库测试会跳过并说明原因）和固定版本的 `govulncheck`（`golang.org/x/vuln/cmd/govulncheck@v1.8.0`，升级时手动改）。
