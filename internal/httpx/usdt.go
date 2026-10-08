package httpx

import (
	"context"
	"dufaka/internal/pay"
	"dufaka/internal/store"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/skip2/go-qrcode"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (a *App) usdtPay(w http.ResponseWriter, r *http.Request, o store.Order) {
	if !a.usdt.Enabled() {
		a.fail(w, r, "USDT 支付暂不可用")
		return
	}
	deadline := o.Created.Add(time.Duration(a.live().Site(r.Context()).ExpireMin) * time.Minute)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := a.live().LockUSDTInvoice(ctx, o, deadline, func(ctx context.Context) (pay.USDTInvoice, error) { return a.usdt.Create(ctx, o.SN, o.Actual) })
	if err != nil {
		a.failErr(w, r, "创建 USDT 支付单", err)
		return
	}
	if err = p.Validate(p.OrderNo, o.Actual, a.usdt.Address); err != nil {
		a.failErr(w, r, "读取 USDT 支付单", err)
		return
	}
	if !strings.HasPrefix(p.OrderNo, "shop-"+o.SN+"-") {
		a.fail(w, r, "支付单与订单不匹配")
		return
	}
	if p.Expires.Before(deadline) {
		deadline = p.Expires
	}
	if !time.Now().Before(deadline) {
		a.fail(w, r, "付款时间已过，请勿继续转账")
		return
	}
	qr, err := qrcode.Encode(p.Address, qrcode.Medium, 256)
	if err != nil {
		a.fail(w, r, "二维码生成失败")
		return
	}
	a.view(w, "usdtpay.html", map[string]any{"Site": a.live().Site(r.Context()), "Order": o, "Invoice": p, "QR": encodePNG(qr), "PayDeadline": deadline.Unix()})
}
func (a *App) syncUSDTOrder(ctx context.Context, o store.Order) store.Order {
	if !a.usdt.Enabled() || (o.Status != 1 && o.Status != -1) {
		return o
	}
	db := a.live()
	channel, err := db.Pay(ctx, o.PayID)
	if err != nil || channel.Check != "usdt" {
		return o
	}
	want, err := db.USDTInvoice(ctx, o.SN)
	if errors.Is(err, pgx.ErrNoRows) {
		return o
	}
	if err != nil {
		log.Printf("USDT quote lookup failed: %v", err)
		return o
	}
	if !strings.HasPrefix(want.OrderNo, "shop-"+o.SN+"-") {
		return o
	}
	key := "usdt:" + o.SN
	receipt, fetched, err := a.receipts.get(ctx, key, func(c context.Context) (pay.Receipt, error) { return a.usdt.Check(c, want, o.Actual, o.Created) })
	if fetched {
		stamp, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if e := db.MarkUSDTPolled(stamp, o.SN); e != nil {
			log.Printf("USDT poll stamp failed: %v", e)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("USDT receipt %s: %v", o.SN, err)
		}
		return o
	}
	if !receipt.Paid || receipt.Amount != int64(o.Actual) {
		return o
	}
	if _, err = db.Complete(ctx, o.SN, o.Actual, "usdt:"+receipt.From); err != nil && !store.IsPaidShort(err) {
		log.Printf("USDT settle %s: %v", o.SN, err)
		return o
	}
	a.receipts.forget(key)
	if fresh, err := db.OrderBySNChecked(ctx, o.SN); err == nil {
		return fresh
	}
	return o
}
func (a *App) SyncUSDT(ctx context.Context) {
	if !a.usdt.Enabled() || a.live() == nil {
		return
	}
	sns, err := a.live().USDTWaiting(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("USDT waiting lookup: %v", err)
		}
		return
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sn := range jobs {
				if ctx.Err() != nil {
					return
				}
				o, err := a.live().OrderBySN(ctx, sn)
				if err == nil {
					a.syncUSDTOrder(ctx, o)
				}
			}
		}()
	}
	for _, sn := range sns {
		select {
		case jobs <- sn:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}
