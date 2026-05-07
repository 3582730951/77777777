// Package i18n keeps a tiny Chinese / English string catalogue. The default
// locale is Chinese (zh-CN); users toggle via the language pill in the topbar
// which writes a `lang` cookie. Templates use the Funcmap entry `T`.
package i18n

type Lang string

const (
	CN Lang = "cn"
	EN Lang = "en"
)

func Normalize(s string) Lang {
	switch s {
	case "en", "EN", "en-US", "en_US":
		return EN
	default:
		return CN
	}
}

// dict is a flat map: key → { lang → text }.
var dict = map[string]map[Lang]string{
	"app.name":              {CN: "LLM 账号池", EN: "LLM Pool"},
	"app.tenant_center":     {CN: "租户中心", EN: "Tenant Center"},
	"nav.dashboard":         {CN: "概览", EN: "Overview"},
	"nav.accounts":          {CN: "账号池", EN: "Accounts"},
	"nav.groups":            {CN: "分组与路由", EN: "Groups & Routing"},
	"nav.keys":              {CN: "API Keys", EN: "API Keys"},
	"nav.tenants":           {CN: "租户", EN: "Tenants"},
	"nav.audit":             {CN: "审计日志", EN: "Audit Log"},
	"nav.cluster":           {CN: "集群", EN: "Cluster"},
	"nav.tenant_dashboard":  {CN: "概览", EN: "Overview"},
	"nav.my_keys":           {CN: "我的 API Keys", EN: "My API Keys"},
	"nav.usage":             {CN: "用量", EN: "Usage"},
	"nav.guide":             {CN: "录入指南", EN: "Enrollment Guide"},
	"nav.tenant_portal":     {CN: "租户使用端 →", EN: "Tenant Portal →"},

	"action.login":          {CN: "登录", EN: "Sign in"},
	"action.logout":         {CN: "退出", EN: "Sign out"},
	"action.add_account":    {CN: "+ 添加账号", EN: "+ Add account"},
	"action.add_group":      {CN: "+ 添加分组", EN: "+ Add group"},
	"action.add_tenant":     {CN: "新建租户", EN: "Create tenant"},
	"action.add_user":       {CN: "添加用户", EN: "Add user"},
	"action.create":         {CN: "创建", EN: "Create"},
	"action.cancel":         {CN: "取消", EN: "Cancel"},
	"action.save":           {CN: "保存", EN: "Save"},
	"action.delete":         {CN: "删除", EN: "Delete"},
	"action.detail":         {CN: "详情", EN: "Detail"},
	"action.probe":          {CN: "探测", EN: "Probe"},
	"action.revoke":         {CN: "吊销", EN: "Revoke"},
	"action.reset_password": {CN: "重置密码", EN: "Reset password"},
	"action.generate":       {CN: "生成", EN: "Generate"},
	"action.toggle_theme":   {CN: "切换主题", EN: "Toggle theme"},
	"action.toggle_lang":    {CN: "中/EN", EN: "中/EN"},
	"action.backup":         {CN: "导出备份", EN: "Export backup"},

	"label.id":              {CN: "ID", EN: "ID"},
	"label.name":            {CN: "名称", EN: "Name"},
	"label.username":        {CN: "用户名", EN: "Username"},
	"label.password":        {CN: "密码", EN: "Password"},
	"label.provider":        {CN: "Provider", EN: "Provider"},
	"label.tenant":          {CN: "租户", EN: "Tenant"},
	"label.group":           {CN: "分组", EN: "Group"},
	"label.label":           {CN: "标签", EN: "Label"},
	"label.state":           {CN: "状态", EN: "State"},
	"label.confidence":      {CN: "置信度", EN: "Confidence"},
	"label.ewma":            {CN: "EWMA", EN: "EWMA"},
	"label.inflight":        {CN: "并发", EN: "In-flight"},
	"label.last_success":    {CN: "最近成功", EN: "Last success"},
	"label.created_at":      {CN: "创建于", EN: "Created"},
	"label.actions":         {CN: "操作", EN: "Actions"},
	"label.source":          {CN: "来源", EN: "Source"},
	"label.optional":        {CN: "可选", EN: "Optional"},

	"kpi.total_accounts":    {CN: "账号总数", EN: "Accounts"},
	"kpi.healthy":           {CN: "健康账号", EN: "Healthy"},
	"kpi.groups":            {CN: "分组", EN: "Groups"},
	"kpi.tenants":           {CN: "租户", EN: "Tenants"},
	"kpi.my_accounts":       {CN: "我的账号", EN: "My accounts"},
	"kpi.my_keys":           {CN: "我的 Keys", EN: "My keys"},

	"chart.requests_1h":     {CN: "最近 1 小时请求量", EN: "Requests (last 1h)"},
	"chart.requests_24h":    {CN: "最近 24 小时请求量", EN: "Requests (last 24h)"},
	"chart.confidence_dist": {CN: "账号置信度分布", EN: "Account confidence"},
	"chart.account_realtime":{CN: "账号实时状态", EN: "Account live status"},
	"chart.cache_overall":   {CN: "缓存命中（24h）", EN: "Cache hit (24h)"},
	"chart.cache_per_key":   {CN: "按 API Key 命中率", EN: "Hit rate by API key"},
	"chart.audit_live":      {CN: "实时事件", EN: "Live events"},
	"chart.cache_hit_rate":  {CN: "缓存命中率", EN: "Cache hit rate"},
	"chart.requests_count":  {CN: "请求数", EN: "Requests"},
	"chart.hits_count":      {CN: "命中数", EN: "Hits"},

	"acct.add_title":        {CN: "添加账号", EN: "Add account"},
	"acct.tls_profile":      {CN: "TLS 指纹 Profile", EN: "TLS Profile"},
	"acct.user_agent":       {CN: "User-Agent", EN: "User-Agent"},
	"acct.proxy":            {CN: "HTTP 代理", EN: "HTTP proxy"},
	"acct.session_token":    {CN: "Session Token", EN: "Session Token"},
	"acct.refresh_token":    {CN: "Refresh Token", EN: "Refresh Token"},
	"acct.cookies":          {CN: "Cookies", EN: "Cookies"},
	"acct.cookies_hint":     {CN: "完整 cookie jar，多行 name=value", EN: "Full cookie jar, line-separated name=value"},

	"tenant.title":          {CN: "租户管理", EN: "Tenants"},
	"tenant.users":          {CN: "登录用户", EN: "Login users"},
	"tenant.password_once":  {CN: "首次显示密码（仅本次可见）", EN: "Password (shown once)"},
	"tenant.add_user_hint":  {CN: "为该租户创建一个登录账号", EN: "Create a login user for this tenant"},

	"portal.login_title":    {CN: "租户使用端", EN: "Tenant Portal"},
	"portal.login_hint":     {CN: "使用管理员分配的账号密码登录", EN: "Sign in with credentials given by your admin"},
	"portal.login_invalid":  {CN: "用户名或密码错误", EN: "Username or password is incorrect"},
	"portal.login_required": {CN: "请填写用户名与密码", EN: "Username and password are required"},
	"portal.create_key":     {CN: "生成新 Key", EN: "Generate new key"},

	"empty.no_accounts":     {CN: "尚无账号。点击右上角 + 添加账号。", EN: "No accounts yet. Click + above."},
	"empty.no_groups":       {CN: "尚未配置任何分组。", EN: "No groups configured."},
	"empty.no_keys":         {CN: "尚无 keys", EN: "No keys yet"},

	"audit.title":           {CN: "审计日志（实时）", EN: "Audit log (live)"},
	"audit.event_stream":    {CN: "事件流", EN: "Event stream"},

	"guide.title":           {CN: "录入真实订阅凭证指南", EN: "Real subscription enrollment guide"},

	"enroll.title":          {CN: "一键录入账号", EN: "One-click enrollment"},
	"enroll.start":          {CN: "开始录入", EN: "Start enrollment"},
	"enroll.bookmarklet":    {CN: "拖到收藏栏 → 在已登录的页面点击", EN: "Drag to bookmark bar → click on the logged-in page"},
	"enroll.bookmarklet_short": {CN: "录入到 LLM Pool", EN: "Enroll to LLM Pool"},
	"enroll.step1":          {CN: "1. 在新标签页打开并登录服务网站", EN: "1. Open the service site in a new tab and sign in"},
	"enroll.step2":          {CN: "2. 把右侧按钮拖到浏览器收藏栏（或复制地址手动添加）", EN: "2. Drag the button to your bookmarks bar (or copy and add manually)"},
	"enroll.step3":          {CN: "3. 在已登录页面点击该收藏，账号会自动回灌", EN: "3. Click the bookmarklet from the logged-in tab; credentials will be captured"},
	"enroll.waiting":        {CN: "等待录入...", EN: "Waiting for enrollment..."},
	"enroll.completed":      {CN: "录入成功 ✓ 跳转中", EN: "Enrollment complete ✓ redirecting"},
	"enroll.expired":        {CN: "已过期，请重新发起", EN: "Expired, please restart"},
	"enroll.failed":         {CN: "失败:", EN: "Failed:"},
	"enroll.note":           {CN: "备注 (可选)", EN: "Note (optional)"},
	"enroll.expires_in":     {CN: "10 分钟内有效", EN: "Valid for 10 minutes"},
	"enroll.alt_paste":      {CN: "或者改用手动粘贴方式", EN: "Or use manual paste instead"},
}

func T(lang Lang, key string) string {
	if m, ok := dict[key]; ok {
		if v, ok := m[lang]; ok && v != "" {
			return v
		}
		if v, ok := m[CN]; ok && v != "" {
			return v
		}
	}
	return key
}
