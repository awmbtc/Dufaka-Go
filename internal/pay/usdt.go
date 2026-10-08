package pay

import (
	"bytes"
	"context"
	"crypto/rand"
	"dufaka/internal/order"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// USDT reuses GoToAI's authenticated TRC20 service. Secrets never reach the page.
type USDT struct {
	Base, Secret, Address string
	HTTP                  *http.Client
}
type USDTInvoice struct {
	ID      string      `json:"id"`
	OrderNo string      `json:"order_no"`
	Status  string      `json:"status"`
	Network string      `json:"network"`
	Token   string      `json:"token"`
	Chain   string      `json:"chain"`
	Address string      `json:"address"`
	CNY     json.Number `json:"cny_amount"`
	Amount  string      `json:"usdt_amount"`
	Expires time.Time   `json:"expires_at"`
	PaidAt  *time.Time  `json:"paid_at"`
	TxID    string      `json:"tx_id"`
}

var usdtID = regexp.MustCompile(`^usdt_[a-f0-9]{24}$`)
var tronAddress = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
var usdtAmount = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{6}$`)
var tronTx = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func (u USDT) Enabled() bool {
	b, err := url.Parse(u.Base)
	return err == nil && b.Host != "" && b.User == nil && b.RawQuery == "" && b.Fragment == "" && (b.Scheme == "https" || b.Scheme == "http" && (b.Hostname() == "127.0.0.1" || b.Hostname() == "localhost")) && strings.TrimSpace(u.Secret) != "" && tronAddress.MatchString(u.Address)
}
func (u USDT) call(ctx context.Context, method, path string, data any) (USDTInvoice, error) {
	var out USDTInvoice
	if !u.Enabled() {
		return out, errors.New("USDT service is not configured")
	}
	var body []byte
	if data != nil {
		var err error
		body, err = json.Marshal(data)
		if err != nil {
			return out, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(u.Base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return out, errors.New("USDT request invalid")
	}
	req.Header.Set("Authorization", "Bearer "+u.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := u.HTTP
	if client == nil {
		client = &http.Client{Timeout: 7 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	res, err := client.Do(req)
	if err != nil {
		return out, errors.New("USDT service unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("USDT service returned HTTP %d", res.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		return out, errors.New("USDT response invalid")
	}
	return out, nil
}
func (u USDT) Create(ctx context.Context, sn string, cents order.Cents) (USDTInvoice, error) {
	// A failed/crashed request can leave an unexposed remote invoice. A fresh attempt
	// gets a new reference; only the invoice committed locally may ever be shown.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return USDTInvoice{}, err
	}
	ref := "shop-" + sn + "-" + hex.EncodeToString(nonce)
	p, err := u.call(ctx, http.MethodPost, "/payments", map[string]any{"order_no": ref, "cny_amount": json.Number(cents.Yuan()), "meta": map[string]string{"source": "clodex-shop", "shop_order": sn}})
	if err != nil {
		return p, err
	}
	if err = p.Validate(ref, cents, u.Address); err != nil {
		return USDTInvoice{}, err
	}
	if p.Status != "pending" || !p.Expires.After(time.Now()) || p.Expires.After(time.Now().Add(24*time.Hour)) {
		return USDTInvoice{}, errors.New("USDT invoice is not payable")
	}
	return p, nil
}
func (p USDTInvoice) Validate(ref string, cents order.Cents, address string) error {
	amount, ok := new(big.Rat).SetString(p.Amount)
	cny, cok := new(big.Rat).SetString(string(p.CNY))
	if !usdtID.MatchString(p.ID) || p.OrderNo != ref || p.Address != address || !tronAddress.MatchString(address) || p.Network != "TRON" || p.Chain != "TRC20" || p.Token != "USDT" || !usdtAmount.MatchString(p.Amount) || !ok || amount.Sign() <= 0 || !cok || cny.Cmp(big.NewRat(int64(cents), 100)) != 0 || p.Expires.IsZero() {
		return errors.New("USDT invoice does not match order")
	}
	return nil
}
func (u USDT) Check(ctx context.Context, want USDTInvoice, cents order.Cents, created time.Time) (Receipt, error) {
	if err := want.Validate(want.OrderNo, cents, u.Address); err != nil {
		return Receipt{}, err
	}
	p, err := u.call(ctx, http.MethodPost, "/payments/"+want.ID+"/check", struct{}{})
	if err != nil {
		return Receipt{}, err
	}
	if err = p.Validate(want.OrderNo, cents, want.Address); err != nil {
		return Receipt{}, err
	}
	if p.ID != want.ID || p.Amount != want.Amount || !p.Expires.Equal(want.Expires) {
		return Receipt{}, errors.New("USDT locked invoice changed")
	}
	if p.Status == "pending" || p.Status == "expired" {
		return Receipt{}, nil
	}
	if p.Status != "paid" || !tronTx.MatchString(p.TxID) || p.PaidAt == nil || p.PaidAt.Before(created.Add(-2*time.Minute)) || p.PaidAt.After(want.Expires.Add(30*time.Minute)) {
		return Receipt{}, errors.New("USDT receipt invalid")
	}
	return Receipt{Paid: true, Amount: int64(cents), From: strings.ToLower(p.TxID), At: *p.PaidAt}, nil
}
