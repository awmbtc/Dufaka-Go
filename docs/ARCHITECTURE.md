# Dufaka-Go 架构

Dufaka-Go 按原发卡站 2.0.6（`assimon/dujiaoka`，提交 `e846d12`）的顾客页面和店主功能，用 Go 与 PostgreSQL 重新实现。页面样式沿用原站的样式文件。仓库名 Dufaka-Go。

当前验收口径（2026-09-23）：前台三套皮肤按原站页面布局和外观核对，保留 Dufaka-Go 品牌。后台保留现有 macOS 27 风格，可修复布局、显示和功能缺失，不对标原站后台外观。下文部分功能为设计目标，实际已实现及缺口以 `AUDIT-2026-09-23.md` 为准。

## 进程

一个 Go 进程同时提供前台、后台、支付通知和定时任务。数据只放 PostgreSQL。不安装 Redis，不另开队列进程。

进程只听本机，nginx 对外提供 HTTPS。安装完成后，安装地址关闭。

```
浏览器 → nginx → Dufaka-Go → PostgreSQL
                 ├── 前台三套模板
                 ├── 后台 /admin
                 ├── 支付通知
                 └── 过期与发信
```

启动配置只有监听地址、数据库连接和会话密钥。站名、模板、邮件、极验和推送都在设置表里，由原来的「系统设置」页修改。

## 目录

```
cmd/dufaka/       进程入口
internal/http/    路由
internal/store/   前台
internal/admin/   后台
internal/order/   下单、计价、发货、过期
internal/pay/     支付渠道
internal/notify/  邮件、推送、商品回调
internal/install/ 安装
internal/db/      数据访问
web/unicorn/      前台模板与静态文件
web/luna/
web/hyper/
web/admin/        后台模板
sql/001_schema.sql
```

三套前台皮肤各自保留。系统设置里切换模板后，下一页使用对应皮肤。后台按原站 Dcat 的菜单、列表、表单和按钮重做，不换成另一套管理界面。

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
| GET | `/check-geetest` | 极验 |
| GET | `/install` | 未安装时可打开 |
| POST | `/do-install` | 执行安装 |
| GET | `/pay-gateway/{handle}/{payway}/{orderSN}` | 转入具体支付 |
| | `/pay/...` | 各渠道的下单、通知和回跳，路径与原站相同 |

下单字段名不变：`gid`、`email`、`payway`、`by_amount`、`search_pwd`、`coupon_code`、`img_verify_code`、极验字段，以及人工商品的额外输入框。

下单成功后写入 cookie `dujiaoka_orders`，值是订单号数组。浏览器查询读这个 cookie。

每个皮肤都包含首页、购买、结算、订单详情、查询、二维码支付和错误页。文案有简体和繁体。站名、logo、关键词、公告、页脚、默认模板和 Google 翻译开关都来自系统设置。

下单校验与原站相同：

- 自动发货商品的库存显示未售卡密数量。人工处理商品用库存数字。
- 单次限购大于 0 时，超过即拒绝。
- 购买数量是大于 0 的整数。库存不足即拒绝。
- 邮箱必填，并且必须是邮箱格式。
- 开启查询密码时，密码不能为空。
- 开启图形验证码或极验时，校验失败即拒绝。
- 商品不存在或未启用即拒绝。
- 该商品有循环卡密时，数量只能是 1。
- 优惠码必须属于该商品、处于启用、剩余次数大于 0。
- 人工商品按配置检查额外输入框。内容写入订单详情，一行一条，形式为「说明:值」。

价格只在服务器计算：

- 总价 = 实际售价 × 数量，保留两位小数。
- 批发按数量阶梯取单价。优惠额 = 原总价 − 批发总价。
- 优惠码是固定减免。
- 实付 = 总价 − 优惠码 − 批发优惠。结果小于等于 0 时记为 0。

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

系统设置仍是四个标签：基本设置、订单推送、邮件、极验。字段名不变，例如 `title`、`template`、`order_expire_time`、`is_open_search_pwd`、`is_open_geetest`、`driver`、`host`。

## 支付

结算页只显示已启用、且场景匹配电脑、手机或全部的渠道。跳转和扫码两种展示不变。

| 标识 | 名称 | 路径 |
| --- | --- | --- |
| zfbf2f | 支付宝当面付 | `/pay/alipay` |
| aliweb | 支付宝 PC | `/pay/alipay` |
| wescan | 微信扫码 | `/pay/wepay` |
| mqq、mzfb、mwx | 码支付 | `/pay/mapay` |
| pszfb、pswx | Paysapi | `/pay/paysapi` |
| payjswescan | Payjs 微信扫码 | `/pay/payjs` |
| alipay、wxpay、qqpay | 易支付 | `/pay/yipay` |
| paypal | PayPal | `/pay/paypal` |
| vzfb、vwx | V 免签 | `/pay/vpay` |
| stripe | Stripe | `/pay/stripe` |
| coinbase | Coinbase | `/pay/coinbase` |
| epusdt | Epusdt | `/pay/epusdt` |
| tokenpay-trx 及各链 | TokenPay | `/pay/tokenpay` |

到账金额必须和订单实付一致，精确到分，才发货。金额不一致不修改订单。

微信扫码仍显示二维码。底下使用现有商户的微信支付 APIv3 Native。原站源码是 APIv2，现有商户不能用那套密钥付款，所以只换协议，不换页面，也不做 H5 自动拉起。

订单已经完成后，如果再次收到金额一致的通知，按该渠道要求回答成功。第一次通知才发货。

## 发货和通知

确认付款与发货在同一个数据库事务中完成。

自动发货时锁定未售卡密。循环卡密不改为已售。普通卡密改为已售，原文写入订单详情，状态改为已完成，并发送 `card_send_user_email`。数量不够时，订单改为异常，详情写库存不足，不把卡密标成已售。

人工处理时状态改为待处理，库存减少购买数量，并向管理员邮箱发送 `manual_send_manage_mail`。

两种成功都会增加销量。然后按开关发送 Server 酱、Telegram、Bark 和企业微信。商品填了回调地址时，以 JSON POST 这些字段：`title`、`order_sn`、`email`、`actual_price`、`order_info`、`good_id`、`gd_name`。

待支付超过 `order_expire_time` 分钟后改为过期，并退回本单占用的优惠券次数，写入 `coupon_ret_back`。过期和退券在同一个事务里完成。

邮件模板标识保持为 `card_send_user_email`、`manual_send_manage_mail`、`pending_order`、`completed_order`、`failed_order`。占位符保持为 `{webname}`、`{weburl}`、`{ord_title}`、`{ord_info}`、`{order_id}`、`{buy_amount}`、`{ord_price}`、`{created_at}`、`{product_name}`。正文重新编写，顾客看到的信息与原站一致。

## 表

金额使用 `numeric(10,2)`。状态使用原来的整数。软删除保留 `deleted_at`。

业务表是 `goods_group`、`goods`、`carmis`、`coupons`、`coupons_goods`、`orders`、`pays`、`emailtpls`。

后台表是 `admin_users`、`admin_roles`、`admin_permissions`、`admin_menu`，以及原站的角色关联表。管理员密码使用 bcrypt。

`settings` 保存系统设置，键与原表单相同。后台仍是原来的系统设置页。配置不再只放在缓存里，因此清缓存不会丢掉站名、模板和邮件参数。

`jobs` 保存待发送的邮件、推送和回调，以及失败原因，由本进程重试。前台没有这个页面。

领取卡密使用行锁：`SELECT … FOR UPDATE SKIP LOCKED`。两人同时买最后一张时，一人成功，另一人得到原来的库存不足异常。普通购买看不出差别。

查询密码和卡密按原站以明文存在订单中。后台可以查看和复制。

## 安装

未安装时，除安装页和静态文件外都进入安装页。安装写入管理员和站点地址。完成后 `/install` 不再接受提交。没有免登录的连接测试。

初始支付渠道和邮件模板与原站种子数据一致，包括默认的启用和停用状态。

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

内部改动只纠正原来会超卖、重复通知被当成失败、以及配置只存在缓存里这三件事。成功路径上的页面和功能不改变。
