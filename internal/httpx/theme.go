package httpx

import (
	"html/template"
	"strings"

	"github.com/microcosm-cc/bluemonday"

	"dufaka/internal/store"
)

func goodsCount(groups []store.Group) int {
	total := 0
	for _, group := range groups {
		total += len(group.Goods)
	}
	return total
}

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

var richPolicy = bluemonday.UGCPolicy()

// Only shop-authored rich text uses this sanitizer. Product names and order data remain escaped.
func rich(text string) template.HTML { return template.HTML(richPolicy.Sanitize(text)) }
