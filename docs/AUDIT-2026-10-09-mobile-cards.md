# 手机商品卡片与输入缩放验收

- 商品卡片填满容器，≤768px 左右留白均为 12px；图片 72px，名称 16px，可换行，价格和余量同组排列，批量优惠独立排列。
- 前台 `touch-action: manipulation` 保留滚动和双指缩放，关闭双击页面缩放；可编辑控件在手机/触控设备上至少 16px，邮箱/优惠码等输入框至少 44px 高。viewport 保留 user-scalable=yes。
- Go 测试、go vet、diff 检查通过；浏览器 320/375/390/430/768/1024/1440px 无页面或卡片横向溢出，手机卡片左右测量均为 12px。
- 本地长标题、批量价、缺货夹具已验收；分类和价格排序正常。邮箱/优惠码填入与双击后 visualViewport.scale=1，输入字号 16px，数量仍为 20px，控制台无错误。
- 验收浏览器为 macOS 内置浏览器的窄视口；没有真实 iPhone 键盘/双指手势设备，因此真机手势结果需后续回验。没有用禁止用户缩放的 viewport 或拦截多指事件。
- 实现依据：[WebKit touch-action 说明](https://webkit.org/blog/5610/more-responsive-tapping-on-ios/)。
- 证据保存于 AIproject/audit-runs/shop-mobile-cards-20261009。本轮不更改订单逻辑、数据或数据库结构。
