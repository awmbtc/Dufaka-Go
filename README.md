# Dufaka-Go

自动发卡商店。一个 Go 进程，数据放在 PostgreSQL。

使用、修改或再分发本软件，必须保留仓库根目录的 MIT 许可声明。详见 [LICENSE](LICENSE)。

## 运行

```bash
psql "$DUFAKA_DATABASE_URL" -f sql/001_schema.sql
export DUFAKA_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/dufaka?sslmode=disable
export DUFAKA_SESSION_KEY="$(openssl rand -hex 32)"
export DUFAKA_BASE_URL=https://shop.example.com
export DUFAKA_ADDR=127.0.0.1:8080
go run ./cmd/dufaka
```

打开 `/install` 创建管理员。后台在 `/admin`。

微信支付扫码使用 APIv3。进程需要这些环境变量：`WECHAT_PAY_APP_ID`、`WECHAT_PAY_MCH_ID`、`WECHAT_PAY_CERT_SERIAL_NO`、`WECHAT_PAY_API_V3_KEY`、`WECHAT_PAY_PRIVATE_KEY`、`WECHAT_PAY_PUBLIC_KEY_ID`、`WECHAT_PAY_PLATFORM_KEY`。支付通知会先校验平台公钥签名，再解密。不要把密钥写进仓库。

## 目前包含

- 前台：首页、购买、下单、结算、订单详情、按订单号 / 邮箱 / 浏览器查询
- 自动发货行锁、过期退回优惠码、过期后到账仍可发货
- 后台：商品、分类、卡密导入、优惠码、订单、支付通道、邮件模板、系统设置
- 微信 Native 收款码在本机生成
- 易支付、码支付、Payjs、Paysapi、V 免签的签名函数

luna、hyper 的独立皮肤，邮件、极验、推送，以及除微信以外的支付跳转页，还没有接上。
