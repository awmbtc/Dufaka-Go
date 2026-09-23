package store

import "context"

func (db *DB) EnsureCldxPay(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO pays (pay_name, pay_check, pay_method, pay_client, merchant_id, merchant_key, merchant_pem, pay_handleroute, is_open, created_at, updated_at)
		SELECT 'cldx', 'cldx', 2, 3, '', '', '', '/pay/cldx', 1, now(), now()
		WHERE NOT EXISTS (SELECT 1 FROM pays WHERE pay_check='cldx') ON CONFLICT (pay_check) DO NOTHING`)
	if err != nil {
		return err
	}
	_, err = db.Pool.Exec(ctx, `ALTER TABLE orders ADD COLUMN IF NOT EXISTS cldx_minor bigint, ADD COLUMN IF NOT EXISTS cldx_expires_at bigint`)
	return err
}

// LockCldxQuote makes the first successful quote authoritative, including concurrent requests.
func (db *DB) LockCldxQuote(ctx context.Context, sn string, minor, expires int64) (int64, int64, error) {
	var amount, exp int64
	err := db.Pool.QueryRow(ctx, `UPDATE orders SET cldx_minor=COALESCE(cldx_minor,$2),
 cldx_expires_at=COALESCE(cldx_expires_at,$3), updated_at=now()
 WHERE order_sn=$1 AND status=1 RETURNING cldx_minor,cldx_expires_at`, sn, minor, expires).Scan(&amount, &exp)
	return amount, exp, err
}

func (db *DB) CldxMinor(ctx context.Context, sn string) (int64, error) {
	var minor *int64
	err := db.Pool.QueryRow(ctx, `SELECT cldx_minor FROM orders WHERE order_sn=$1`, sn).Scan(&minor)
	if err != nil || minor == nil {
		return 0, err
	}
	return *minor, nil
}

func (db *DB) WaitingSNs(ctx context.Context, check string) ([]string, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT o.order_sn FROM orders o
		JOIN pays p ON p.id = o.pay_id
		WHERE o.status IN (1,-1) AND o.cldx_minor IS NOT NULL AND p.pay_check=$1`, check)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sn string
		if err := rows.Scan(&sn); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}
