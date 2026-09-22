package admin

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var moneyRe = regexp.MustCompile(`^\d+(\.\d{1,2})?$`)

func money(s string, allowEmpty bool) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if allowEmpty {
			return "0.00", nil
		}
		return "", errors.New("不能为空")
	}
	if !moneyRe.MatchString(s) {
		return "", errors.New("格式不正确")
	}
	whole, frac, ok := strings.Cut(s, ".")
	if !ok {
		frac = "00"
	} else if len(frac) == 1 {
		frac += "0"
	}
	trimmed := strings.TrimLeft(whole, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	if len(trimmed) > 8 {
		return "", errors.New("超出范围")
	}
	return whole + "." + frac, nil
}

type goodForm struct {
	ID, GroupID, InStock, Sales, Ord, BuyLimit, Type, Open int
	Name, Desc, Keywords, Picture, Retail, Actual          string
	Prompt, Detail, Wholesale, Other, Hook                 string
}

func parseIntField(raw, label string, empty int, allowEmpty bool) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if allowEmpty {
			return empty, nil
		}
		return 0, errors.New("请填写" + label)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(label + "必须是整数")
	}
	return n, nil
}

func readGoodForm(r *http.Request) (goodForm, error) {
	var f goodForm
	var err error
	if f.GroupID, err = parseIntField(r.FormValue("group_id"), "所属分类", 0, false); err != nil {
		return f, err
	}
	f.Name = strings.TrimSpace(r.FormValue("gd_name"))
	f.Desc = strings.TrimSpace(r.FormValue("gd_description"))
	f.Keywords = strings.TrimSpace(r.FormValue("gd_keywords"))
	f.Picture = strings.TrimSpace(r.FormValue("picture"))
	f.Retail, err = money(r.FormValue("retail_price"), true)
	if err != nil {
		return f, errors.New("零售价" + err.Error())
	}
	f.Actual, err = money(r.FormValue("actual_price"), false)
	if err != nil {
		return f, errors.New("实际售价" + err.Error())
	}
	if f.InStock, err = parseIntField(r.FormValue("in_stock"), "库存", 0, true); err != nil {
		return f, err
	}
	if f.Sales, err = parseIntField(r.FormValue("sales_volume"), "销量", 0, true); err != nil {
		return f, err
	}
	if f.Ord, err = parseIntField(r.FormValue("ord"), "排序权重", 1, true); err != nil {
		return f, err
	}
	if f.BuyLimit, err = parseIntField(r.FormValue("buy_limit_num"), "单次限购", 0, true); err != nil {
		return f, err
	}
	f.Prompt = normalizeNL(r.FormValue("buy_prompt"))
	f.Detail = normalizeNL(r.FormValue("description"))
	if f.Type, err = parseIntField(r.FormValue("type"), "商品类型", 0, false); err != nil {
		return f, err
	}
	f.Wholesale = normalizeNL(r.FormValue("wholesale_price_cnf"))
	f.Other = normalizeNL(r.FormValue("other_ipu_cnf"))
	f.Hook = normalizeNL(r.FormValue("api_hook"))
	if f.Open, err = parseIntField(r.FormValue("is_open"), "是否上架", 0, false); err != nil {
		return f, err
	}
	return f, validateGood(f)
}

func validateGood(f goodForm) error {
	if f.GroupID < 1 {
		return errors.New("请选择所属分类")
	}
	if f.Name == "" {
		return errors.New("请填写商品名称")
	}
	if f.Desc == "" {
		return errors.New("请填写商品描述")
	}
	if f.Keywords == "" {
		return errors.New("请填写商品关键字")
	}
	if runeLen(f.Name) > 200 || runeLen(f.Desc) > 200 || runeLen(f.Keywords) > 200 {
		return errors.New("商品名称、描述或关键字超过 200 字")
	}
	if f.Type != 1 && f.Type != 2 {
		return errors.New("商品类型必须是 1（自动发货）或 2（人工处理）")
	}
	if f.Open != 0 && f.Open != 1 {
		return errors.New("是否上架取值不正确")
	}
	if f.InStock < 0 {
		return errors.New("库存不能为负数")
	}
	if f.Sales < 0 {
		return errors.New("销量不能为负数")
	}
	if f.BuyLimit < 0 {
		return errors.New("单次限购不能为负数")
	}
	return nil
}

// parseCards splits an import box into one card per line.
// Empty lines are dropped. dedupe keeps the first copy of each line.
func parseCards(text string, dedupe bool) ([]string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if runeLen(line) > 10000 {
			return nil, errors.New("单条卡密过长")
		}
		if dedupe {
			if _, ok := seen[line]; ok {
				continue
			}
			seen[line] = struct{}{}
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil, errors.New("请填写需要导入的卡密")
	}
	if len(out) > 5000 {
		return nil, errors.New("一次最多导入 5000 条")
	}
	return out, nil
}
