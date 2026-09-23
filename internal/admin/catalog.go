package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type goodRow struct {
	ID                            int
	Name, Desc, Keywords, Picture string
	GroupName, Retail, Actual     string
	TypeName, ShelfName, Created  string
	Stock, Sales, Ord, Type, Open int
	Trashed                       bool
}

type goodsPage struct {
	View
	Rows    []goodRow
	Groups  []opt
	Name    string
	Type    string
	GroupID string
	Trashed bool
}

type goodsFormPage struct {
	View
	Form    goodForm
	Groups  []opt
	Action  string
	Editing bool
}

func (s *Server) goodsList(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	groups, err := s.nameOptions(r, `SELECT id, gp_name FROM goods_group WHERE deleted_at IS NULL ORDER BY ord DESC, id DESC`)
	if err != nil {
		s.fail(w, u, "goods", dbErr(err))
		return
	}
	where := []string{}
	args := []any{}
	if trashed(r) {
		where = append(where, "g.deleted_at IS NOT NULL")
	} else {
		where = append(where, "g.deleted_at IS NULL")
	}
	name := strings.TrimSpace(r.URL.Query().Get("gd_name"))
	if name != "" {
		args = append(args, "%"+name+"%")
		where = append(where, "g.gd_name ILIKE $"+strconv.Itoa(len(args)))
	}
	typ := r.URL.Query().Get("type")
	if typ == "1" || typ == "2" {
		args = append(args, typ)
		where = append(where, "g.type = $"+strconv.Itoa(len(args)))
	}
	gid := r.URL.Query().Get("group_id")
	if id, err := strconv.Atoi(gid); err == nil && id > 0 {
		args = append(args, id)
		where = append(where, "g.group_id = $"+strconv.Itoa(len(args)))
	}
	page, limit := pageArgs(r)
	args = append(args, limit+1, (page-1)*limit)
	q := `SELECT g.id, g.gd_name, g.gd_description, g.gd_keywords, COALESCE(g.picture,''),
		COALESCE(gg.gp_name,''), COALESCE(g.retail_price,0)::text, g.actual_price::text,
		CASE WHEN g.type=1 THEN (
			SELECT count(*)::int FROM carmis c WHERE c.goods_id=g.id AND c.status=1 AND c.deleted_at IS NULL
		) ELSE g.in_stock END,
		COALESCE(g.sales_volume,0), COALESCE(g.ord,1), g.type, g.is_open,
		to_char(COALESCE(g.created_at, now()), 'YYYY-MM-DD HH24:MI'),
		g.deleted_at IS NOT NULL
		FROM goods g
		LEFT JOIN goods_group gg ON gg.id=g.group_id
		WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY g.id DESC LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.db().Query(r.Context(), q, args...)
	if err != nil {
		s.fail(w, u, "goods", dbErr(err))
		return
	}
	defer rows.Close()
	var list []goodRow
	for rows.Next() {
		var row goodRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Desc, &row.Keywords, &row.Picture, &row.GroupName, &row.Retail, &row.Actual, &row.Stock, &row.Sales, &row.Ord, &row.Type, &row.Open, &row.Created, &row.Trashed); err != nil {
			s.fail(w, u, "goods", dbErr(err))
			return
		}
		row.TypeName = goodsTypeName(row.Type)
		row.ShelfName = shelfName(row.Open)
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "goods", dbErr(err))
		return
	}
	hasNext := len(list) > limit
	if hasNext {
		list = list[:limit]
	}
	prev, next := pageLinks(r, page, hasNext)
	p := goodsPage{
		View: s.shell(u, "商品列表", "goods", r), Rows: list, Groups: groups,
		Name: name, Type: typ, GroupID: gid, Trashed: trashed(r),
	}
	p.Prev, p.Next = prev, next
	s.render(w, http.StatusOK, "goods_list", p)
}

func goodsTypeName(v int) string {
	if v == 1 {
		return "自动发货"
	}
	return "人工处理"
}

func shelfName(v int) string {
	if v == 1 {
		return "上架"
	}
	return "下架"
}

func (s *Server) goodsCreate(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	s.goodsForm(w, r, u, goodForm{Type: 1, Open: 1, Ord: 1, Retail: "0.00", Actual: "0.00"}, "", http.StatusOK)
}

func (s *Server) goodsEdit(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := s.loadGood(r, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.fail(w, u, "goods", "记录不存在")
			return
		}
		s.fail(w, u, "goods", dbErr(err))
		return
	}
	s.goodsForm(w, r, u, f, "", http.StatusOK)
}

func (s *Server) goodsForm(w http.ResponseWriter, r *http.Request, u session, f goodForm, msg string, status int) {
	groups, err := s.nameOptions(r, `SELECT id, gp_name FROM goods_group WHERE deleted_at IS NULL ORDER BY ord DESC, id DESC`)
	if err != nil {
		s.fail(w, u, "goods", dbErr(err))
		return
	}
	title := "新增商品"
	action := "/admin/goods"
	if f.ID > 0 {
		title = "编辑商品"
		action = "/admin/goods/" + strconv.Itoa(f.ID)
	}
	if msg == "" && len(groups) == 0 {
		msg = "请先添加商品分类"
	}
	p := goodsFormPage{View: s.shell(u, title, "goods", nil), Form: f, Groups: groups, Action: action, Editing: f.ID > 0}
	p.Err = msg
	s.render(w, status, "goods_form", p)
}

func (s *Server) loadGood(r *http.Request, id int) (goodForm, error) {
	var f goodForm
	err := s.db().QueryRow(r.Context(), `
		SELECT id, group_id, gd_name, gd_description, gd_keywords, COALESCE(picture,''),
			COALESCE(retail_price,0)::text, actual_price::text, in_stock, COALESCE(sales_volume,0),
			COALESCE(ord,1), buy_limit_num, COALESCE(buy_prompt,''), COALESCE(description,''), type,
			COALESCE(wholesale_price_cnf,''), COALESCE(other_ipu_cnf,''), COALESCE(api_hook,''), is_open
		FROM goods WHERE id=$1 AND deleted_at IS NULL`, id).Scan(
		&f.ID, &f.GroupID, &f.Name, &f.Desc, &f.Keywords, &f.Picture,
		&f.Retail, &f.Actual, &f.InStock, &f.Sales, &f.Ord, &f.BuyLimit, &f.Prompt, &f.Detail, &f.Type,
		&f.Wholesale, &f.Other, &f.Hook, &f.Open)
	return f, err
}

func (s *Server) goodsSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, _ := pathID(r)
	f, err := readGoodForm(r)
	f.ID = id
	if err != nil {
		s.goodsForm(w, r, u, f, err.Error(), http.StatusBadRequest)
		return
	}
	var n int
	err = s.db().QueryRow(r.Context(), `SELECT count(*) FROM goods_group WHERE id=$1 AND deleted_at IS NULL`, f.GroupID).Scan(&n)
	if err != nil {
		s.goodsForm(w, r, u, f, dbErr(err), http.StatusBadRequest)
		return
	}
	if n == 0 {
		s.goodsForm(w, r, u, f, "所属分类不存在", http.StatusBadRequest)
		return
	}
	if id == 0 {
		_, err = s.db().Exec(r.Context(), `
			INSERT INTO goods (group_id, gd_name, gd_description, gd_keywords, picture,
				retail_price, actual_price, in_stock, sales_volume, ord, buy_limit_num,
				buy_prompt, description, type, wholesale_price_cnf, other_ipu_cnf, api_hook,
				is_open, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,now(),now())`,
			f.GroupID, f.Name, f.Desc, f.Keywords, f.Picture, f.Retail, f.Actual, f.InStock, f.Sales, f.Ord,
			f.BuyLimit, f.Prompt, f.Detail, f.Type, f.Wholesale, f.Other, f.Hook, f.Open)
	} else {
		var tag int64
		tag, err = execCount(s, r, `
			UPDATE goods SET group_id=$1, gd_name=$2, gd_description=$3, gd_keywords=$4, picture=$5,
				retail_price=$6, actual_price=$7, in_stock=$8, sales_volume=$9, ord=$10, buy_limit_num=$11,
				buy_prompt=$12, description=$13, type=$14, wholesale_price_cnf=$15, other_ipu_cnf=$16,
				api_hook=$17, is_open=$18, updated_at=now()
			WHERE id=$19 AND deleted_at IS NULL`,
			f.GroupID, f.Name, f.Desc, f.Keywords, f.Picture, f.Retail, f.Actual, f.InStock, f.Sales, f.Ord,
			f.BuyLimit, f.Prompt, f.Detail, f.Type, f.Wholesale, f.Other, f.Hook, f.Open, id)
		if err == nil && tag == 0 {
			err = errors.New("记录不存在或已删除")
		}
	}
	if err != nil {
		s.goodsForm(w, r, u, f, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/goods", "保存成功")
}

func execCount(s *Server, r *http.Request, sql string, args ...any) (int64, error) {
	tag, err := s.db().Exec(r.Context(), sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Server) goodsDelete(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "goods", "goods", false)
}

func (s *Server) goodsRestore(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "goods", "goods", true)
}

func (s *Server) markDeleted(w http.ResponseWriter, r *http.Request, u session, table, nav string, restore bool) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok || !allowedTable(table) {
		http.NotFound(w, r)
		return
	}
	// table comes from allowedTable, not from the request.
	q := `UPDATE ` + table + ` SET deleted_at=now(), updated_at=now() WHERE id=$1 AND deleted_at IS NULL`
	msg := "已删除"
	back := "/admin/" + listPath(table)
	if restore {
		q = `UPDATE ` + table + ` SET deleted_at=NULL, updated_at=now() WHERE id=$1 AND deleted_at IS NOT NULL`
		msg = "已恢复"
		back += "?trashed=1"
	}
	if table == "carmis" {
		q += " AND reserved_order_id IS NULL AND status=1"
	}
	n, err := execCount(s, r, q, id)
	if err != nil {
		s.fail(w, u, nav, dbErr(err))
		return
	}
	if n == 0 {
		s.fail(w, u, nav, "记录不存在")
		return
	}
	redirectOK(w, r, back, msg)
}

func allowedTable(table string) bool {
	switch table {
	case "goods", "goods_group", "carmis", "coupons", "orders":
		return true
	default:
		return false
	}
}

func listPath(table string) string {
	switch table {
	case "goods_group":
		return "groups"
	case "carmis":
		return "carmis"
	case "coupons":
		return "coupons"
	case "orders":
		return "orders"
	default:
		return "goods"
	}
}

type groupRow struct {
	ID, Ord, Open int
	Name, Created string
	Trashed       bool
}

type groupsPage struct {
	View
	Rows    []groupRow
	Form    groupRow
	Action  string
	Trashed bool
}

func (s *Server) groupsPage(w http.ResponseWriter, r *http.Request, u session) {
	s.renderGroups(w, r, u, groupRow{Open: 1, Ord: 1}, "/admin/groups", "", http.StatusOK)
}

func (s *Server) groupEdit(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var g groupRow
	err := s.db().QueryRow(r.Context(), `
		SELECT id, gp_name, is_open, ord, to_char(COALESCE(created_at, now()), 'YYYY-MM-DD HH24:MI')
		FROM goods_group WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&g.ID, &g.Name, &g.Open, &g.Ord, &g.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "groups", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "groups", dbErr(err))
		return
	}
	s.renderGroups(w, r, u, g, "/admin/groups/"+strconv.Itoa(id), "", http.StatusOK)
}

func (s *Server) renderGroups(w http.ResponseWriter, r *http.Request, u session, form groupRow, action, msg string, status int) {
	if !s.ready(w, u) {
		return
	}
	where := "deleted_at IS NULL"
	if trashed(r) {
		where = "deleted_at IS NOT NULL"
	}
	rows, err := s.db().Query(r.Context(), `
		SELECT id, gp_name, is_open, ord, to_char(COALESCE(created_at, now()), 'YYYY-MM-DD HH24:MI'), deleted_at IS NOT NULL
		FROM goods_group WHERE `+where+` ORDER BY id DESC LIMIT 200`)
	if err != nil {
		s.fail(w, u, "groups", dbErr(err))
		return
	}
	defer rows.Close()
	var list []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.ID, &g.Name, &g.Open, &g.Ord, &g.Created, &g.Trashed); err != nil {
			s.fail(w, u, "groups", dbErr(err))
			return
		}
		list = append(list, g)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "groups", dbErr(err))
		return
	}
	title := "商品分类"
	if form.ID > 0 {
		title = "编辑分类"
	}
	p := groupsPage{View: s.shell(u, title, "groups", r), Rows: list, Form: form, Action: action, Trashed: trashed(r)}
	p.Err = msg
	s.render(w, status, "groups", p)
}

func readGroup(r *http.Request) (groupRow, error) {
	g := groupRow{
		Name: strings.TrimSpace(r.FormValue("gp_name")),
		Open: mustInt(r, "is_open", 0),
		Ord:  mustInt(r, "ord", 1),
	}
	if strings.TrimSpace(r.FormValue("ord")) == "" {
		g.Ord = 1
	}
	if g.Name == "" {
		return g, errors.New("请填写分类名称")
	}
	if runeLen(g.Name) > 200 {
		return g, errors.New("分类名称超过 200 字")
	}
	if g.Open != 0 && g.Open != 1 {
		return g, errors.New("是否启用不正确")
	}
	return g, nil
}

func (s *Server) groupSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, _ := pathID(r)
	g, err := readGroup(r)
	g.ID = id
	action := "/admin/groups"
	if id > 0 {
		action = "/admin/groups/" + strconv.Itoa(id)
	}
	if err != nil {
		s.renderGroups(w, r, u, g, action, err.Error(), http.StatusBadRequest)
		return
	}
	if id == 0 {
		_, err = s.db().Exec(r.Context(), `INSERT INTO goods_group (gp_name, is_open, ord, created_at, updated_at) VALUES ($1,$2,$3,now(),now())`, g.Name, g.Open, g.Ord)
	} else {
		var n int64
		n, err = execCount(s, r, `UPDATE goods_group SET gp_name=$1, is_open=$2, ord=$3, updated_at=now() WHERE id=$4 AND deleted_at IS NULL`, g.Name, g.Open, g.Ord, id)
		if err == nil && n == 0 {
			err = errors.New("记录不存在或已删除")
		}
	}
	if err != nil {
		s.renderGroups(w, r, u, g, action, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/groups", "保存成功")
}

func (s *Server) groupDelete(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "goods_group", "groups", false)
}

func (s *Server) groupRestore(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "goods_group", "groups", true)
}

type carmiRow struct {
	ID                       int
	GoodsName, Text, Created string
	Status, Loop             int
	Trashed                  bool
}

type carmisPage struct {
	View
	Rows    []carmiRow
	Goods   []opt
	GoodsID string
	Status  string
	Trashed bool
}

type carmiImportPage struct {
	View
	Goods   []opt
	GoodsID int
	Loop    int
	Dedupe  bool
	Text    string
}

type carmiFormPage struct {
	View
	Goods                 []opt
	ID                    int
	GoodsID, Status, Loop int
	Text                  string
	Action                string
}

func (s *Server) autoGoods(r *http.Request) ([]opt, error) {
	return s.nameOptions(r, `SELECT id, gd_name FROM goods WHERE type=1 AND deleted_at IS NULL ORDER BY id DESC`)
}

func (s *Server) carmisList(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	goods, err := s.autoGoods(r)
	if err != nil {
		s.fail(w, u, "carmis", dbErr(err))
		return
	}
	where := []string{}
	args := []any{}
	if trashed(r) {
		where = append(where, "c.deleted_at IS NOT NULL")
	} else {
		where = append(where, "c.deleted_at IS NULL")
	}
	gid := r.URL.Query().Get("goods_id")
	if id, err := strconv.Atoi(gid); err == nil && id > 0 {
		args = append(args, id)
		where = append(where, "c.goods_id=$"+strconv.Itoa(len(args)))
	}
	st := r.URL.Query().Get("status")
	if st == "1" || st == "2" {
		args = append(args, st)
		where = append(where, "c.status=$"+strconv.Itoa(len(args)))
	}
	page, limit := pageArgs(r)
	args = append(args, limit+1, (page-1)*limit)
	q := `SELECT c.id, COALESCE(g.gd_name,''), c.status, c.is_loop, c.carmi,
		to_char(COALESCE(c.created_at, now()), 'YYYY-MM-DD HH24:MI'), c.deleted_at IS NOT NULL
		FROM carmis c LEFT JOIN goods g ON g.id=c.goods_id
		WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY c.id DESC LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.db().Query(r.Context(), q, args...)
	if err != nil {
		s.fail(w, u, "carmis", dbErr(err))
		return
	}
	defer rows.Close()
	var list []carmiRow
	for rows.Next() {
		var row carmiRow
		if err := rows.Scan(&row.ID, &row.GoodsName, &row.Status, &row.Loop, &row.Text, &row.Created, &row.Trashed); err != nil {
			s.fail(w, u, "carmis", dbErr(err))
			return
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, u, "carmis", dbErr(err))
		return
	}
	hasNext := len(list) > limit
	if hasNext {
		list = list[:limit]
	}
	prev, next := pageLinks(r, page, hasNext)
	p := carmisPage{View: s.shell(u, "卡密列表", "carmis", r), Rows: list, Goods: goods, GoodsID: gid, Status: st, Trashed: trashed(r)}
	p.Prev, p.Next = prev, next
	s.render(w, http.StatusOK, "carmis_list", p)
}

func (s *Server) carmisImportForm(w http.ResponseWriter, r *http.Request, u session) {
	s.renderImport(w, r, u, carmiImportPage{}, "", http.StatusOK)
}

func (s *Server) renderImport(w http.ResponseWriter, r *http.Request, u session, form carmiImportPage, msg string, status int) {
	if !s.ready(w, u) {
		return
	}
	goods, err := s.autoGoods(r)
	if err != nil {
		s.fail(w, u, "import", dbErr(err))
		return
	}
	form.View = s.shell(u, "导入卡密", "import", nil)
	form.Err = msg
	form.Goods = goods
	s.render(w, status, "carmis_import", form)
}

func (s *Server) carmisImport(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	form := carmiImportPage{
		GoodsID: mustInt(r, "goods_id", 0),
		Loop:    mustInt(r, "is_loop", 0),
		Dedupe:  r.FormValue("remove_duplication") == "1",
		Text:    r.FormValue("carmis_list"),
	}
	cards, err := parseCards(form.Text, form.Dedupe)
	if err != nil {
		s.renderImport(w, r, u, form, err.Error(), http.StatusBadRequest)
		return
	}
	if form.Loop != 0 && form.Loop != 1 {
		s.renderImport(w, r, u, form, "循环卡密取值不正确", http.StatusBadRequest)
		return
	}
	var goodsID int
	err = s.db().QueryRow(r.Context(), `SELECT id FROM goods WHERE id=$1 AND type=1 AND deleted_at IS NULL`, form.GoodsID).Scan(&goodsID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderImport(w, r, u, form, "请选择自动发货商品", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.renderImport(w, r, u, form, dbErr(err), http.StatusBadRequest)
		return
	}
	tx, err := s.db().Begin(r.Context())
	if err != nil {
		s.renderImport(w, r, u, form, dbErr(err), http.StatusBadRequest)
		return
	}
	defer tx.Rollback(r.Context())
	for _, card := range cards {
		if _, err = tx.Exec(r.Context(), `INSERT INTO carmis (goods_id, status, is_loop, carmi, created_at, updated_at) VALUES ($1,1,$2,$3,now(),now())`, goodsID, form.Loop, card); err != nil {
			s.renderImport(w, r, u, form, dbErr(err), http.StatusBadRequest)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.renderImport(w, r, u, form, dbErr(err), http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/carmis", "导入卡密成功")
}

func (s *Server) carmiEdit(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := carmiFormPage{Action: "/admin/carmis/" + strconv.Itoa(id), ID: id}
	err := s.db().QueryRow(r.Context(), `SELECT goods_id, status, is_loop, carmi FROM carmis WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&p.GoodsID, &p.Status, &p.Loop, &p.Text)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, u, "carmis", "记录不存在")
		return
	}
	if err != nil {
		s.fail(w, u, "carmis", dbErr(err))
		return
	}
	s.renderCarmi(w, r, u, p, "", http.StatusOK)
}

func (s *Server) renderCarmi(w http.ResponseWriter, r *http.Request, u session, p carmiFormPage, msg string, status int) {
	goods, err := s.autoGoods(r)
	if err != nil {
		s.fail(w, u, "carmis", dbErr(err))
		return
	}
	p.View = s.shell(u, "编辑卡密", "carmis", nil)
	p.Err = msg
	p.Goods = goods
	s.render(w, status, "carmi_form", p)
}

func (s *Server) carmiSave(w http.ResponseWriter, r *http.Request, u session) {
	if !s.ready(w, u) {
		return
	}
	if !parseForm(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := carmiFormPage{
		ID: id, Action: "/admin/carmis/" + strconv.Itoa(id),
		GoodsID: mustInt(r, "goods_id", 0),
		Status:  mustInt(r, "status", 0),
		Loop:    mustInt(r, "is_loop", 0),
		Text:    strings.TrimSpace(r.FormValue("carmi")),
	}
	if p.Text == "" {
		s.renderCarmi(w, r, u, p, "请填写卡密内容", http.StatusBadRequest)
		return
	}
	if p.Status != 1 && p.Status != 2 {
		s.renderCarmi(w, r, u, p, "状态不正确", http.StatusBadRequest)
		return
	}
	if p.Loop != 0 && p.Loop != 1 {
		s.renderCarmi(w, r, u, p, "循环卡密取值不正确", http.StatusBadRequest)
		return
	}
	var goodsID int
	err := s.db().QueryRow(r.Context(), `SELECT id FROM goods WHERE id=$1 AND type=1 AND deleted_at IS NULL`, p.GoodsID).Scan(&goodsID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderCarmi(w, r, u, p, "请选择自动发货商品", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.renderCarmi(w, r, u, p, dbErr(err), http.StatusBadRequest)
		return
	}
	n, err := execCount(s, r, `UPDATE carmis SET goods_id=$1, status=$2, is_loop=$3, carmi=$4, updated_at=now() WHERE id=$5 AND deleted_at IS NULL AND reserved_order_id IS NULL AND status=1`, p.GoodsID, p.Status, p.Loop, p.Text, id)
	if err != nil {
		s.renderCarmi(w, r, u, p, dbErr(err), http.StatusBadRequest)
		return
	}
	if n == 0 {
		s.renderCarmi(w, r, u, p, "记录不存在、已删除、已售出或正被订单占用", http.StatusBadRequest)
		return
	}
	redirectOK(w, r, "/admin/carmis", "保存成功")
}

func (s *Server) carmiDelete(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "carmis", "carmis", false)
}

func (s *Server) carmiRestore(w http.ResponseWriter, r *http.Request, u session) {
	s.markDeleted(w, r, u, "carmis", "carmis", true)
}
