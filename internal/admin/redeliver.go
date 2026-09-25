package admin

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"dufaka/internal/store"
)

// redeliver is the single call site for re-running delivery of a status-6
// order. It is a variable so tests can stub it. The real work lives in
// store.(*DB).Redeliver, the same settlement path Complete uses.
var redeliver = func(ctx context.Context, pool *pgxpool.Pool, sn string) (bool, error) {
	return (&store.DB{Pool: pool}).Redeliver(ctx, sn)
}
