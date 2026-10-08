package store

import (
	"context"
	"dufaka/internal/pay"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

func (db *DB) USDTInvoice(ctx context.Context, sn string) (pay.USDTInvoice, error) {
	var raw []byte
	err := db.Pool.QueryRow(ctx, `SELECT usdt_payment FROM orders WHERE order_sn=$1 AND deleted_at IS NULL`, sn).Scan(&raw)
	if err != nil {
		return pay.USDTInvoice{}, err
	}
	if len(raw) == 0 {
		return pay.USDTInvoice{}, pgx.ErrNoRows
	}
	var p pay.USDTInvoice
	err = json.Unmarshal(raw, &p)
	return p, err
}

// Serialize quote creation across tabs/processes. Nothing is shown before commit.
func (db *DB) LockUSDTInvoice(ctx context.Context, o Order, deadline time.Time, create func(context.Context) (pay.USDTInvoice, error)) (pay.USDTInvoice, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return pay.USDTInvoice{}, err
	}
	defer tx.Rollback(context.Background())
	var raw []byte
	var status int
	err = tx.QueryRow(ctx, `SELECT usdt_payment,status FROM orders WHERE order_sn=$1 AND deleted_at IS NULL FOR UPDATE`, o.SN).Scan(&raw, &status)
	if err != nil {
		return pay.USDTInvoice{}, err
	}
	if status != 1 || !time.Now().Before(deadline) {
		return pay.USDTInvoice{}, errors.New("order no longer payable")
	}
	var p pay.USDTInvoice
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &p)
	} else {
		p, err = create(ctx)
		if err == nil {
			raw, err = json.Marshal(p)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE orders SET usdt_payment=$2 WHERE order_sn=$1`, o.SN, raw)
		}
	}
	if err != nil {
		return pay.USDTInvoice{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return pay.USDTInvoice{}, err
	}
	return p, nil
}
func (db *DB) USDTWaiting(ctx context.Context) ([]string, error) {
	rows, err := db.Pool.Query(ctx, `SELECT o.order_sn FROM orders o JOIN pays p ON p.id=o.pay_id WHERE p.pay_check='usdt' AND o.status IN(1,-1) AND o.deleted_at IS NULL AND o.usdt_payment IS NOT NULL AND o.created_at>$1 ORDER BY o.usdt_polled_at NULLS FIRST,o.id LIMIT 100`, time.Now().Add(-48*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sns []string
	for rows.Next() {
		var sn string
		if err = rows.Scan(&sn); err != nil {
			return nil, err
		}
		sns = append(sns, sn)
	}
	return sns, rows.Err()
}
func (db *DB) MarkUSDTPolled(ctx context.Context, sn string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE orders SET usdt_polled_at=$2 WHERE order_sn=$1`, sn, time.Now().Unix())
	return err
}
