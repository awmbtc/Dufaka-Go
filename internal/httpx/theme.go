package httpx

import (
	"encoding/json"
	"html/template"
	"strings"

	"dufaka/internal/store"
)

func lunaGoods(groups []store.Group) template.JS {
	type good struct {
		ID        int    `json:"id"`
		InStock   int    `json:"in_stock"`
		Picture   string `json:"picture"`
		GdName    string `json:"gd_name"`
		Actual    string `json:"actual_price"`
		Wholesale string `json:"wholesale_price_cnf"`
		Sales     int    `json:"sales_volume"`
	}
	type group struct {
		Key    int    `json:"key"`
		GpName string `json:"gp_name"`
		Goods  []good `json:"goods"`
	}
	out := make([]group, 0, len(groups))
	for i, g := range groups {
		gg := group{Key: i, GpName: g.Name, Goods: []good{}}
		for _, item := range g.Goods {
			pic := item.Picture
			if pic == "" {
				pic = "/assets/common/images/default.jpg"
			}
			gg.Goods = append(gg.Goods, good{
				ID: item.ID, InStock: item.InStock, Picture: pic, GdName: item.Name,
				Actual: item.Actual.Yuan(), Sales: item.Sales,
				Wholesale: strings.ReplaceAll(item.Wholesale, "\n", "\r\n"),
			})
		}
		out = append(out, gg)
	}
	raw, _ := json.Marshal(out)
	return template.JS(raw)
}
