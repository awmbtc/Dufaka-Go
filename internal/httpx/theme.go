package httpx

import (
	"encoding/json"
	"html/template"
	"strings"

	"dufaka/internal/store"
)

func stockPercent(inStock, sales int) int {
	if inStock+sales <= 0 {
		return 0
	}
	return inStock * 100 / (inStock + sales)
}

func wholesaleRows(raw string) [][2]string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var rows [][2]string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		rows = append(rows, [2]string{strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])})
	}
	return rows
}

// ExtraInput is one manual-delivery field from other_ipu_cnf (field=label=required).
type ExtraInput struct {
	Field, Label string
	Required     bool
}

func extraInputs(raw string) []ExtraInput {
	var out []ExtraInput
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "=")
		if len(parts) < 3 || parts[0] == "" {
			continue
		}
		req := parts[2] == "1" || strings.EqualFold(parts[2], "true")
		out = append(out, ExtraInput{Field: parts[0], Label: parts[1], Required: req})
	}
	return out
}

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
