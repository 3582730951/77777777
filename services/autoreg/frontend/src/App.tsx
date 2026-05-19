import { BrowserRouter, Link, NavLink, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { useEffect, useState } from 'react'
import { getPlatforms } from '@/lib/app-data'
import { getAuthToken, setAuthToken, API, cn } from '@/lib/utils'
import Dashboard from '@/pages/Dashboard'
import Accounts from '@/pages/Accounts'
import Register from '@/pages/Register'
import Proxies from '@/pages/Proxies'
import SettingsPage from '@/pages/SettingsPage'
import TaskHistory from '@/pages/TaskHistory'
import UpdateBanner from '@/components/UpdateBanner'
import {
  ChevronRight,
  History,
  LayoutDashboard,
  Moon,
  Settings as SettingsIcon,
  Sun,
  Monitor,
  UserPlus,
  Users,
  PanelLeftClose,
  PanelLeft,
} from 'lucide-react'

/* ------------------------------------------------------------------ */
/*  Sidebar                                                            */
/* ------------------------------------------------------------------ */

type NavItem = { path: string; label: string; icon: any; exact?: boolean }

const NAV_ITEMS: NavItem[] = [
  { path: '/', label: '总览', icon: LayoutDashboard, exact: true },
  { path: '/register', label: '注册', icon: UserPlus },
  { path: '/history', label: '任务', icon: History },
]

function Sidebar({
  theme,
  toggleTheme,
  collapsed,
  setCollapsed,
}: {
  theme: string
  toggleTheme: () => void
  collapsed: boolean
  setCollapsed: (v: boolean) => void
}) {
  const location = useLocation()
  const navigate = useNavigate()
  const [platforms, setPlatforms] = useState<{ key: string; label: string }[]>([])
  const [accountsOpen, setAccountsOpen] = useState(location.pathname.startsWith('/accounts'))
  const [focusWithin, setFocusWithin] = useState(false)
  const visualCollapsed = collapsed && !focusWithin

  useEffect(() => {
    getPlatforms()
      .then((data) => setPlatforms((data || []).map((p: any) => ({ key: p.name, label: p.display_name }))))
      .catch(() => setPlatforms([]))
  }, [])

  useEffect(() => {
    if (location.pathname.startsWith('/accounts')) setAccountsOpen(true)
  }, [location.pathname])

  const isAccounts = location.pathname.startsWith('/accounts')
  const isSettings = location.pathname === '/settings'

  const navLinkClass = (active: boolean) =>
    cn(
      'group flex items-center gap-3 rounded-lg px-3 py-2 text-[13px] font-medium transition-colors',
      'max-md:min-w-[72px] max-md:flex-col max-md:justify-center max-md:gap-1 max-md:px-2 max-md:py-1.5 max-md:text-[11px]',
      active
        ? 'bg-[var(--accent-soft)] text-[var(--text-primary)]'
        : 'text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] hover:text-[var(--text-primary)]',
      visualCollapsed && 'md:justify-center md:px-0'
    )

  const iconClass = (active: boolean) =>
    cn(
      'h-[18px] w-[18px] shrink-0 max-md:h-5 max-md:w-5',
      active ? 'text-[var(--accent)]' : 'text-[var(--text-muted)] group-hover:text-[var(--text-secondary)]'
    )

  return (
    <aside
      onFocusCapture={() => setFocusWithin(true)}
      onBlurCapture={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setFocusWithin(false)
      }}
      className={cn(
        'group',
        'fixed inset-x-0 bottom-0 z-40 flex h-16 flex-row border-t border-[var(--border)] bg-[var(--bg-surface)] shadow-[0_-14px_32px_rgba(0,0,0,0.22)] focus-within:ring-1 focus-within:ring-[var(--accent-edge)]',
        'md:sticky md:inset-auto md:top-0 md:h-screen md:flex-col md:border-r md:border-t-0 md:shadow-none md:transition-[width] md:duration-200',
        visualCollapsed ? 'md:w-16' : 'md:w-[220px]'
      )}
    >
      {/* Header */}
      <div className={cn('hidden h-12 shrink-0 items-center border-b border-[var(--border)] px-3 md:flex', visualCollapsed && 'justify-center')}>
        {!visualCollapsed && (
          <div className="flex items-center gap-2.5 min-w-0 flex-1">
            <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-[var(--accent)] text-[11px] font-bold text-white">
              A
            </div>
            <span className="truncate text-sm font-semibold text-[var(--text-primary)]">Auto Register</span>
          </div>
        )}
        {visualCollapsed && (
          <div className="flex h-7 w-7 items-center justify-center rounded-lg bg-[var(--accent)] text-[11px] font-bold text-white">
            A
          </div>
        )}
      </div>

      {/* Nav */}
      <nav aria-label="主导航" className="flex flex-1 items-center gap-1 overflow-x-auto overflow-y-hidden px-2 py-2 md:block md:space-y-0.5 md:overflow-y-auto md:px-2 md:py-3">
        {NAV_ITEMS.map(({ path, label, icon: Icon, exact }) => {
          const active = exact ? location.pathname === path : location.pathname.startsWith(path)
          return (
            <NavLink key={path} to={path} end={exact} className={navLinkClass(active)} title={visualCollapsed ? label : undefined}>
              <Icon className={iconClass(active)} />
              <span className={cn(visualCollapsed && 'md:hidden')}>{label}</span>
            </NavLink>
          )
        })}

        {/* Accounts with sub-items */}
        <div>
          <button type="button"
            aria-expanded={!visualCollapsed && accountsOpen}
            aria-controls="accounts-subnav"
            onClick={() => {
              const isMobile = window.matchMedia('(max-width: 767px)').matches
              if (visualCollapsed || isMobile) {
                navigate('/accounts')
              } else {
                setAccountsOpen(!accountsOpen)
              }
            }}
            className={cn(navLinkClass(isAccounts), 'w-full')}
            title={visualCollapsed ? '账号' : undefined}
          >
            <Users className={iconClass(isAccounts)} />
            <span className={cn('flex-1 text-left max-md:flex-none max-md:text-center', visualCollapsed && 'md:hidden')}>账号</span>
            {!visualCollapsed && (
              <>
                <ChevronRight className={cn('hidden h-3 w-3 text-[var(--text-muted)] transition-transform duration-150 md:block', accountsOpen && 'rotate-90')} />
              </>
            )}
          </button>
          {!visualCollapsed && accountsOpen && (
            <div id="accounts-subnav" className="ml-[21px] mt-0.5 hidden space-y-px border-l border-[var(--border)] pl-3 md:block">
              {platforms.map((p) => (
                <NavLink
                  key={p.key}
                  to={`/accounts/${p.key}`}
                  className={({ isActive }) =>
                    cn(
                      'block rounded-md px-2.5 py-1.5 text-[13px] transition-colors',
                      isActive
                        ? 'text-[var(--text-primary)] font-medium bg-[var(--bg-hover)]'
                        : 'text-[var(--text-muted)] hover:text-[var(--text-secondary)] hover:bg-[var(--bg-hover)]'
                    )
                  }
                >
                  {p.label}
                </NavLink>
              ))}
            </div>
          )}
        </div>

        {/* Divider */}
        {!visualCollapsed && <div className="!my-2 mx-1 hidden border-t border-[var(--border)] md:block" />}

        {/* Settings with sub-items */}
        <div>
          <button type="button"
            aria-expanded={!visualCollapsed && isSettings}
            aria-controls="settings-subnav"
            onClick={() => {
              if (visualCollapsed) {
                navigate('/settings')
              } else {
                navigate('/settings')
              }
            }}
            className={cn(navLinkClass(isSettings), 'w-full')}
            title={visualCollapsed ? '设置' : undefined}
          >
            <SettingsIcon className={iconClass(isSettings)} />
            <span className={cn(visualCollapsed && 'md:hidden')}>设置</span>
          </button>
          {!visualCollapsed && isSettings && (
            <div id="settings-subnav" className="ml-[21px] mt-0.5 hidden space-y-px border-l border-[var(--border)] pl-3 md:block">
              {[
                { label: '通用', hash: 'general' },
                { label: '注册策略', hash: 'register' },
                { label: '邮箱服务', hash: 'mailbox' },
                { label: '验证服务', hash: 'captcha' },
                { label: '接码服务', hash: 'sms' },
                { label: '代理资源', hash: 'proxies' },
                { label: 'ChatGPT', hash: 'chatgpt' },
                { label: '高级', hash: 'advanced' },
                { label: '关于', hash: 'about' },
              ].map((item) => {
                const params = new URLSearchParams(location.search)
                const currentTab = params.get('tab') || 'general'
                const active = currentTab === item.hash
                return (
                  <Link
                    key={item.hash}
                    to={`/settings?tab=${item.hash}`}
                    aria-current={active ? 'page' : undefined}
                    className={cn(
                      'relative block rounded-md px-2.5 py-1.5 text-[13px] transition-colors',
                      active
                        ? 'text-[var(--accent)] font-medium bg-[var(--accent-soft)]'
                        : 'text-[var(--text-muted)] hover:text-[var(--text-secondary)] hover:bg-[var(--bg-hover)]'
                    )}
                  >
                    {active && <span className="absolute -left-[13.5px] top-1/2 -translate-y-1/2 h-4 w-[2px] rounded-full bg-[var(--accent)]" />}
                    {item.label}
                  </Link>
                )
              })}
            </div>
          )}
        </div>
      </nav>

      {/* Footer */}
      <div className="hidden shrink-0 items-center gap-1 border-t border-[var(--border)] px-2 py-1.5 md:flex">
        <button type="button"
          onClick={toggleTheme}
          aria-label="切换主题"
          className={cn(
            'flex items-center justify-center rounded-md p-2 text-[var(--text-muted)] transition-colors hover:bg-[var(--bg-hover)] hover:text-[var(--text-secondary)]',
          )}
          title={theme === 'light' ? '切换到深色' : theme === 'dark' ? '切换到浅色' : '跟随系统'}
        >
          {theme === 'light' ? <Moon className="h-4 w-4" /> : theme === 'system' ? <Monitor className="h-4 w-4" /> : <Sun className="h-4 w-4" />}
        </button>
        {!visualCollapsed && (
          <span className="flex-1 text-[12px] text-[var(--text-muted)]">
            {theme === 'light' ? '浅色' : theme === 'dark' ? '深色' : '系统'}
          </span>
        )}
        <button type="button"
          onClick={() => setCollapsed(!collapsed)}
          aria-label={collapsed ? '展开侧栏' : '收起侧栏'}
          className="flex items-center justify-center rounded-md p-2 text-[var(--text-muted)] transition-colors hover:bg-[var(--bg-hover)] hover:text-[var(--text-secondary)]"
          title={collapsed ? '展开侧栏' : '收起侧栏'}
        >
          {collapsed ? <PanelLeft className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
        </button>
      </div>
    </aside>
  )
}

/* ------------------------------------------------------------------ */
/*  Shell                                                              */
/* ------------------------------------------------------------------ */

function Shell({
  theme,
  setTheme,
  toggleTheme,
}: {
  theme: string
  setTheme: (t: string) => void
  toggleTheme: () => void
}) {
  const [collapsed, setCollapsed] = useState(() => localStorage.getItem('sidebar-collapsed') === 'true')

  useEffect(() => {
    localStorage.setItem('sidebar-collapsed', String(collapsed))
  }, [collapsed])

  return (
    <div className="min-h-screen bg-[var(--bg-base)] md:flex md:h-screen md:overflow-hidden">
      <Sidebar theme={theme} toggleTheme={toggleTheme} collapsed={collapsed} setCollapsed={setCollapsed} />
      <main className="min-h-screen pb-20 md:min-h-0 md:flex-1 md:overflow-y-auto md:pb-0">
        <div className="mx-auto max-w-6xl px-4 py-4 sm:px-6 sm:py-6 lg:px-8">
          <UpdateBanner />
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/accounts" element={<Accounts />} />
            <Route path="/accounts/:platform" element={<Accounts />} />
            <Route path="/register" element={<Register />} />
            <Route path="/history" element={<TaskHistory />} />
            <Route path="/proxies" element={<Proxies />} />
            <Route path="/settings" element={<SettingsPage theme={theme} setTheme={setTheme} />} />
          </Routes>
        </div>
      </main>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/*  Login                                                              */
/* ------------------------------------------------------------------ */

function LoginScreen({ onLogin }: { onLogin: (token: string) => void }) {
  const [pw, setPw] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError('')
    try {
      const res = await fetch(API + '/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: pw }),
      })
      const data = await res.json()
      if (data.ok) {
        setAuthToken(data.token || '')
        onLogin(data.token || '')
      } else {
        setError(data.error || '密码错误')
      }
    } catch {
      setError('请求失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex h-screen items-center justify-center bg-[var(--bg-base)]">
      <form onSubmit={submit} className="w-80 space-y-4 rounded-xl border border-[var(--border)] bg-[var(--bg-card)] p-6">
        <div className="flex items-center gap-2.5">
          <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-[var(--accent)] text-sm font-bold text-white">A</div>
          <h1 className="text-base font-semibold text-[var(--text-primary)]">Any Auto Register</h1>
        </div>
        <p className="text-sm text-[var(--text-muted)]">请输入访问密码</p>
        <label htmlFor="autoreg-login-password" className="sr-only">访问密码</label>
        <input
          id="autoreg-login-password"
          type="password"
          value={pw}
          onChange={(e) => setPw(e.target.value)}
          placeholder="密码"
          autoFocus
          className="control-surface w-full"
        />
        {error && <p className="text-xs text-[var(--state-danger)]">{error}</p>}
        <button type="submit"
          disabled={loading || !pw}
          className="w-full rounded-lg bg-[var(--accent)] px-4 py-2.5 text-sm font-medium text-white transition-colors hover:bg-[var(--accent-hover)] disabled:opacity-50"
        >
          {loading ? '验证中...' : '登 录'}
        </button>
      </form>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/*  App root                                                           */
/* ------------------------------------------------------------------ */

export default function App() {
  const [theme, setTheme] = useState(() => {
    const saved = localStorage.getItem('llm_pool_theme') || localStorage.getItem('theme') || 'system'
    const normalized = saved === 'auto' ? 'system' : saved
    return ['light', 'dark', 'system'].includes(normalized) ? normalized : 'system'
  })
  const [authState, setAuthState] = useState<'loading' | 'open' | 'locked' | 'authed'>('loading')

  useEffect(() => {
    const normalizedTheme = theme === 'auto' ? 'system' : theme
    const applyTheme = () => {
      let effective = normalizedTheme
      if (normalizedTheme === 'system') {
        effective = window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
      }
      document.documentElement.classList.toggle('light', effective === 'light')
      document.documentElement.dataset.theme = normalizedTheme
    }
    applyTheme()
    localStorage.setItem('theme', normalizedTheme)
    localStorage.setItem('llm_pool_theme', normalizedTheme === 'system' ? 'auto' : normalizedTheme)
    const mq = window.matchMedia('(prefers-color-scheme: light)')
    const handler = () => { if (normalizedTheme === 'system') applyTheme() }
    mq.addEventListener('change', handler)
    return () => mq.removeEventListener('change', handler)
  }, [theme])

  useEffect(() => {
    fetch(API + '/auth/check')
      .then((r) => r.json())
      .then((data) => {
        if (!data.required) setAuthState('open')
        else if (getAuthToken()) setAuthState('authed')
        else setAuthState('locked')
      })
      .catch(() => setAuthState('open'))
  }, [])

  const toggleTheme = () =>
    setTheme((c) => (c === 'dark' ? 'light' : c === 'light' ? 'system' : 'dark'))

  if (authState === 'loading') {
    return <div className="flex h-screen items-center justify-center bg-[var(--bg-base)] text-[var(--text-muted)] text-sm">加载中...</div>
  }
  if (authState === 'locked') {
    return <LoginScreen onLogin={() => setAuthState('authed')} />
  }

  return (
    <BrowserRouter basename={import.meta.env.BASE_URL.replace(/\/$/, '') || undefined}>
      <Shell theme={theme} setTheme={setTheme} toggleTheme={toggleTheme} />
    </BrowserRouter>
  )
}
