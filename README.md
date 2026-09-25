# Dufaka-Go

自动发卡商店。一个 Go 进程，数据放在 PostgreSQL。

使用、修改或再分发本软件，必须保留仓库根目录的 MIT 许可声明。详见 [LICENSE](LICENSE)。

## 手工安装

先准备好 PostgreSQL。不需要 Redis，也不需要自己导入 SQL。

```bash
export DUFAKA_ADDR=127.0.0.1:8080
export DUFAKA_BASE_URL=http://127.0.0.1:8080
go run ./cmd/dufaka
```

浏览器打开 http://127.0.0.1:8080/install 。页面上有四步：启动、连接数据库、建表、管理员。可以先点「测试连接」，再点「开始安装」。安装会创建数据库（账号有权限时）、数据表、管理员、微信扫码渠道和五封邮件模板，并把连接写入 `dufaka.json`。完成后页面给出前台和后台入口。

已经用环境变量提供 `DUFAKA_DATABASE_URL` 时，重启也会读取 `dufaka.json` 里的会话密钥。

微信支付扫码使用 APIv3。配置来自两处：后台「支付通道」里 `wescan` 那一行（商户 ID = 商户号、商户密钥 = 商户 API 私钥（PEM）、商户 KEY = APIv3 密钥），以及只能用环境变量提供的证书级参数：`WECHAT_PAY_APP_ID`、`WECHAT_PAY_CERT_SERIAL_NO`、`WECHAT_PAY_PLATFORM_KEY`（平台公钥或平台证书）、`WECHAT_PAY_PLATFORM_SERIAL`、`WECHAT_PAY_PUBLIC_KEY_ID`。后台行里填了的值永远优先；后台某项留空时才读对应环境变量：商户 KEY → `WECHAT_PAY_API_V3_KEY`，商户 ID → `WECHAT_PAY_MCH_ID`，商户密钥 → `WECHAT_PAY_PRIVATE_KEY`。后两个变量已不推荐（请改填后台），但仍然支持，老部署不用改。APIv3 密钥必须正好 32 字节（ASCII）（首尾的换行、空格会被去掉，多一个或少一个字节都不收）。

收银台在调用微信之前做一次完整自检：缺项、商户私钥和平台公钥能否解析、APIv3 密钥是否 32 字节。任何一项不通过，商品页就不显示微信支付（日志里说明原因，每个渠道只打一次），下单也会被拒绝；已有订单进入收银台时，顾客只会看到「微信支付暂未配置完整，请联系店主」。微信下单接口报错时，顾客只看到「微信支付下单失败，请稍后再试或联系店主」，具体原因和渠道编号只写日志。后台「支付通道」页能看到缺项清单（私钥、平台公钥能否解析目前以日志为准）。cldx 同理：未配置钱包（`WALLET_MERCHANT_SECRET` 等）时商品页不显示 cldx。

支付通知的处理顺序：时间戳窗口（5 分钟，在查库前就做）→ 平台公钥 RSA 验签 → 用 `wescan` 渠道的 APIv3 密钥解密（`pay_check` 唯一，只有一行）。验签是唯一的放行标准，`Wechatpay-Serial` 头只用来解释问题：期望的序列号依次取 `WECHAT_PAY_PLATFORM_SERIAL` → `WECHAT_PAY_PLATFORM_KEY` 是 X.509 平台证书时它自己的序列号（按整字节大写十六进制）→ `WECHAT_PAY_PUBLIC_KEY_ID`，比较时不分大小写、忽略前导 0。验签通过但序列号不一致只记一条警告；验签失败且序列号不一致时，日志写「微信支付平台证书可能已轮换：通知序列号 X，已配置 Y」，据此更新平台公钥即可，和伪造请求（「签名无效」）能分开。验签通过但解密失败回 500，微信会重试，改好 APIv3 密钥后自动补上；已收款但库存不足的订单记为「异常」（状态 6）并回微信成功，日志提示补货后在后台重新发货。不要把密钥写进仓库。

`DUFAKA_DATABASE_URL` 手写时密码必须做百分号编码（如 `p@ss` 写成 `p%40ss`）：pgx 5.11 只认到第一个 `@`，密码里的 `@`、`/`、`?`、`#` 不编码会连错库或连不上。安装页生成的连接串已经编码好。运行要求 Go ≥ 1.26.6。

部署要求：nginx 反代配置见 [docs/nginx-shop.conf.example](docs/nginx-shop.conf.example)，至少要传 `X-Real-IP $remote_addr`、`X-Forwarded-For $proxy_add_x_forwarded_for`、`X-Forwarded-Proto $scheme` 和 `Host`。只有对端是可信反代时才读这些头：默认只信本机回环地址（nginx 与本程序在同一台机器）；反代在别的机器（内网、负载均衡、容器网络）时，用 `DUFAKA_TRUSTED_PROXIES` 列出它的地址或网段，逗号分隔，如 `DUFAKA_TRUSTED_PROXIES=10.0.0.0/8,192.168.1.5`（进程启动后读一次）。买家 IP 只读一个头：`DUFAKA_REAL_IP_HEADER` 指定的那个，默认 `X-Real-IP`（nginx 用 `$remote_addr` 覆盖它，客户端伪造不了）；只配了 `X-Forwarded-For` 的反代设成 `DUFAKA_REAL_IP_HEADER=X-Forwarded-For`，此时只取最后一段（反代自己追加的那段），不是合法 IP 一律忽略；HSTS 只在直连 HTTPS 或可信反代声明 `X-Forwarded-Proto: https` 时发送。订单查询（按订单号 / 邮箱、订单详情页）每个 IP 5 分钟内最多 30 次，超出显示「查询过于频繁，请稍后再试」；收银台状态轮询单独计数，每 5 分钟 120 次，每 5 秒轮询一次的收银页不会被限。买家本浏览器下的订单（带签名的订单 Cookie）查看详情和轮询状态都不计数，付款后跳转的发货页不会被限；地址表占满时新地址不会被拒：查询共用一个溢出额度，轮询直接放行。**上线前务必确认线上 nginx 已按示例传 `X-Real-IP` / `X-Forwarded-For`**：可信反代没传客户端地址时，所有买家都会显示为反代地址，程序会停用查询限流并在日志里提示一次「反代未传 X-Real-IP，查询限流已停用」，后台登录则改用一个所有人共用、每 15 分钟 50 次的尝试额度。不开查询密码时，按邮箱查到的订单号只显示末 4 位。静态目录不再列目录，且 `.`/`._` 开头的文件一律 404；从 Mac 上传请用 `rsync --exclude '._*'` 或 `COPYFILE_DISABLE=1`。cldx 收款二维码现在只带 `/cldx/pay?order=<订单号>`，付款参数由服务端按订单锁定的报价重建后再跳到 `clodex://pay`。cldx 到账查询按订单缓存 10 秒（收银页每 5 秒轮询一次，只会打一次钱包），后台每 30 秒一轮、最多 8 个并发、按最久没问过的订单先问。

## 目前包含

- 前台：首页、购买、下单、结算、订单详情、按订单号 / 邮箱 / 浏览器查询
- 自动发货行锁、过期退回优惠码、过期后到账仍可发货
- 后台：商品、分类、卡密导入、优惠码、订单、支付通道、邮件模板、系统设置
- 微信 Native 收款码在本机生成
- cldx 钱包收款（二维码 / 应用深链，后台自动核对到账）

Unicorn、Luna、Hyper 三套前台及 cldx 四参数付款已在本地实现。目前能收款的只有 `wescan`（微信扫码）和 `cldx`；其余渠道和开关的接通状态、已修复项与验证范围见 [审核报告](docs/AUDIT-2026-09-25.md)。
