package httpx

import (
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"dufaka/internal/store"
)

func (a *App) cldxPay(w http.ResponseWriter, r *http.Request, o store.Order) {
	if !a.wallet.Enabled() {
		a.fail(w, r, "cldx 支付还没有配置")
		return
	}
	ttl := time.Duration(a.live().Site(r.Context()).ExpireMin) * time.Minute
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	amount, err := a.live().CldxMinor(r.Context(), o.SN)
	if err != nil {
		a.fail(w, r, "cldx 报价失败")
		return
	}
	if amount <= 0 {
		amount, err = a.wallet.Quote(r.Context(), int64(o.Actual))
	}
	if err != nil || amount <= 0 {
		a.fail(w, r, "cldx 报价失败")
		return
	}
	amount, exp, err := a.live().LockCldxQuote(r.Context(), o.SN, amount, o.Created.Add(ttl).Unix())
	if err != nil {
		a.fail(w, r, "订单状态已改变，请重新查询")
		return
	}
	if exp <= time.Now().Unix() {
		a.fail(w, r, "订单已过期")
		return
	}
	query := url.Values{"to": {a.wallet.MerchantID}, "amount": {strconv.FormatInt(amount, 10)}, "order": {o.SN}, "exp": {strconv.FormatInt(exp, 10)}}.Encode()
	link := a.base + "/cldx/pay?" + query
	// This fixed scheme and encoded parameters are constructed here, never from arbitrary input.
	appLink := template.URL("clodex://pay?" + query)
	direct := appClient(r)
	var png string
	if !direct {
		raw, err := qrcode.Encode(link, qrcode.Medium, 256)
		if err != nil {
			a.fail(w, r, "二维码生成失败")
			return
		}
		png = encodePNG(raw)
	}
	a.view(w, "cldxpay.html", map[string]any{
		"Title": "cldx 支付", "Site": a.live().Site(r.Context()), "Order": o,
		"PayDeadline": exp, "Cldx": formatCldx(amount), "QR": png, "Direct": direct, "AppLink": appLink,
	})
}

func (a *App) cldxLanding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	http.Redirect(w, r, "clodex://pay?checkout="+id, http.StatusFound)
}

func (a *App) cldxLaunch(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "clodex://pay?"+r.URL.RawQuery, http.StatusFound)
}

func (a *App) syncCldxOrder(r *http.Request, o store.Order) store.Order {
	if !a.wallet.Enabled() || (o.Status != 1 && o.Status != -1) {
		return o
	}
	p, err := a.live().Pay(r.Context(), o.PayID)
	if err != nil || p.Check != "cldx" {
		return o
	}
	want, err := a.live().CldxMinor(r.Context(), o.SN)
	if err != nil || want <= 0 {
		return o
	}
	item, err := a.wallet.Receipt(r.Context(), a.wallet.MerchantID, o.SN)
	if err != nil || !item.Paid || item.Refunded != 0 || item.Amount != want {
		return o
	}
	if _, err := a.live().Complete(r.Context(), o.SN, o.Actual, "cldx:"+o.SN); err != nil {
		return o
	}
	next, err := a.live().OrderBySN(r.Context(), o.SN)
	if err != nil {
		return o
	}
	return next
}

func (a *App) SyncCldx(ctx context.Context) {
	if !a.wallet.Enabled() {
		return
	}
	db := a.live()
	if db == nil {
		return
	}
	sns, err := db.WaitingSNs(ctx, "cldx")
	if err != nil {
		return
	}
	for _, sn := range sns {
		o, err := db.OrderBySN(ctx, sn)
		if err != nil {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		_ = a.syncCldxOrder(req, o)
	}
}

func appClient(r *http.Request) bool {
	if c, err := r.Cookie("clodex_app"); err == nil && c.Value == "1" {
		return true
	}
	return r.URL.Query().Get("clodex_app") == "1"
}

func encodePNG(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}

func formatCldx(minor int64) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	text := fmt.Sprintf("%d.%04d", minor/10000, minor%10000)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	return sign + text
}
