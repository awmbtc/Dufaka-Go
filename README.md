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

微信支付扫码使用 APIv3。进程需要这些环境变量：`WECHAT_PAY_APP_ID`、`WECHAT_PAY_MCH_ID`、`WECHAT_PAY_CERT_SERIAL_NO`、`WECHAT_PAY_API_V3_KEY`、`WECHAT_PAY_PRIVATE_KEY`、`WECHAT_PAY_PUBLIC_KEY_ID`、`WECHAT_PAY_PLATFORM_KEY`。支付通知会先校验平台公钥签名，再解密。不要把密钥写进仓库。

## 目前包含

- 前台：首页、购买、下单、结算、订单详情、按订单号 / 邮箱 / 浏览器查询
- 自动发货行锁、过期退回优惠码、过期后到账仍可发货
- 后台：商品、分类、卡密导入、优惠码、订单、支付通道、邮件模板、系统设置
- 微信 Native 收款码在本机生成
- 易支付、码支付、Payjs、Paysapi、V 免签的签名函数

luna、hyper 的独立皮肤，邮件、极验、推送，以及除微信以外的支付跳转页，还没有接上。
