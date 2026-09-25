# 小店界面约定

2026-09-23 用户确认：前台对照 dujiaoka 2.0.6 三套主题的结构与外观，保留 Dufaka-Go 自有品牌。后台保留现有 macOS 27 风格，可以修复布局和功能，不能改成 dujiaoka 后台。

- 前台：Unicorn 使用 Bootstrap；Luna 使用 layui 和原有渐变/分类卡；Hyper 使用其原有商品网格、分栏与卡片。跨主题功能用各自页面壳承载，不把 Bootstrap 结构放入 Luna。
- 后台：`web/assets/admin/settings.css` 是现有样式来源，侧栏、圆角分组、浅/深/系统外观保持。大表格在容器内部横向滚动。
- 富文本只用于站点公告、页脚、商品详情与购买说明，并经白名单清洗；商品名称和订单数据继续转义。
- 收银页明确区分待付款、过期、到账、处理异常和网络失败；复制只有成功后才显示“已复制”。
- 审核前先加载个人 ux-designer 技能；浏览器验证桌面 1440px、手机 390px，覆盖真实交互，不能只看模板能否编译。
- 复用 `TestExportAuditPages` 可通过 `DUFAKA_AUDIT_OUTPUT` 导出测试页面。业务集成测试用 `DUFAKA_TEST_DATABASE_URL` 指向专用本地测试库；测试自动建立和清理独立 schema。
