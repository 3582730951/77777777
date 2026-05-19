import { useEffect, useState } from 'react'
import { getPlatforms } from '@/lib/app-data'
import { apiFetch } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { getTaskStatusText, TASK_STATUS_VARIANTS } from '@/lib/tasks'
import { RefreshCw, Activity, CheckCircle2, AlertTriangle, Clock3, ChevronDown } from 'lucide-react'

function shortId(id: string) {
  if (!id) return '-'
  return id.length > 12 ? '...' + id.slice(-8) : id
}

function formatError(error: string | null | undefined): string {
  if (!error) return ''
  // Try to extract a readable message from JSON-like strings
  try {
    if (error.startsWith('{') || error.startsWith('[')) {
      const parsed = JSON.parse(error)
      if (parsed.message) return parsed.message
      if (parsed.error) return parsed.error
      if (Array.isArray(parsed.errors) && parsed.errors.length > 0) {
        const first = parsed.errors[0]
        return first.message || first.kind || JSON.stringify(first).slice(0, 80)
      }
    }
  } catch {
    // not JSON
  }
  // Truncate long strings
  return error.length > 100 ? error.slice(0, 100) + '...' : error
}

export default function TaskHistory() {
  const [tasks, setTasks] = useState<any[]>([])
  const [platform, setPlatform] = useState('')
  const [status, setStatus] = useState('')
  const [platforms, setPlatforms] = useState<any[]>([])
  const [loading, setLoading] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ page: '1', page_size: '50' })
      if (platform) params.set('platform', platform)
      if (status) params.set('status', status)
      const data = await apiFetch(`/tasks?${params}`)
      setTasks(data.items || [])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    getPlatforms()
      .then((data) => setPlatforms(data || []))
      .catch(() => setPlatforms([]))
  }, [])

  useEffect(() => {
    load()
  }, [platform, status])

  const succeeded = tasks.filter((t) => t.status === 'succeeded').length
  const failed = tasks.filter((t) => t.status === 'failed').length
  const running = tasks.filter((t) =>
    ['running', 'claimed', 'pending', 'cancel_requested'].includes(t.status)
  ).length

  const metricCards = [
    { label: '任务数', value: tasks.length, icon: Activity, tone: 'text-[var(--accent)]' },
    { label: '成功', value: succeeded, icon: CheckCircle2, tone: 'text-[var(--state-success)]' },
    { label: '失败', value: failed, icon: AlertTriangle, tone: 'text-[var(--state-danger)]' },
    { label: '进行中', value: running, icon: Clock3, tone: 'text-[var(--state-warning)]' },
  ]

  return (
    <div className="space-y-5">
      {/* Header */}
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold text-[var(--text-primary)]">任务记录</h1>
        <Button variant="outline" size="sm" onClick={load} disabled={loading}>
          <RefreshCw className={`h-3.5 w-3.5 mr-1.5 ${loading ? 'animate-spin' : ''}`} />
          刷新
        </Button>
      </div>

      {/* Metrics */}
      <div className="grid gap-3 grid-cols-2 lg:grid-cols-4">
        {metricCards.map(({ label, value, icon: Icon, tone }) => (
          <div
            key={label}
            className="flex items-center gap-3 rounded-xl border border-[var(--border)] bg-[var(--bg-card)] px-4 py-3"
          >
            <div className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-[var(--chip-bg)] ${tone}`}>
              <Icon className="h-4 w-4" />
            </div>
            <div>
              <div className="text-[11px] text-[var(--text-muted)] uppercase tracking-wider">{label}</div>
              <div className="text-lg font-semibold text-[var(--text-primary)]">{value}</div>
            </div>
          </div>
        ))}
      </div>

      {/* Filters — inline with table header */}
      <div className="rounded-xl border border-[var(--border)] bg-[var(--bg-card)] overflow-hidden">
        <div className="flex flex-col gap-3 border-b border-[var(--border)] px-4 py-3 sm:flex-row sm:items-center sm:py-2.5">
          <span className="text-sm font-medium text-[var(--text-primary)]">最近任务</span>
          <div className="hidden flex-1 sm:block" />
          <div className="grid grid-cols-1 gap-2 sm:flex sm:items-center">
            <div className="relative min-w-0">
              <select
                value={platform}
                onChange={(e) => setPlatform(e.target.value)}
                className="h-8 w-full appearance-none rounded-md border border-[var(--border)] bg-[var(--bg-input)] pl-3 pr-7 text-xs text-[var(--text-secondary)] transition-colors hover:border-[var(--accent)] focus:border-[var(--accent)] sm:w-auto"
              >
                <option value="">全部平台</option>
                {platforms.map((item: any) => (
                  <option key={item.name} value={item.name}>{item.display_name}</option>
                ))}
              </select>
              <ChevronDown className="pointer-events-none absolute right-2 top-1/2 h-3 w-3 -translate-y-1/2 text-[var(--text-muted)]" />
            </div>
            <div className="relative min-w-0">
              <select
                value={status}
                onChange={(e) => setStatus(e.target.value)}
                className="h-8 w-full appearance-none rounded-md border border-[var(--border)] bg-[var(--bg-input)] pl-3 pr-7 text-xs text-[var(--text-secondary)] transition-colors hover:border-[var(--accent)] focus:border-[var(--accent)] sm:w-auto"
              >
                <option value="">全部状态</option>
                <option value="running">运行中</option>
                <option value="succeeded">成功</option>
                <option value="failed">失败</option>
                <option value="cancelled">已取消</option>
                <option value="interrupted">已中断</option>
              </select>
              <ChevronDown className="pointer-events-none absolute right-2 top-1/2 h-3 w-3 -translate-y-1/2 text-[var(--text-muted)]" />
            </div>
            {(platform || status) && (
              <button
                onClick={() => { setPlatform(''); setStatus('') }}
                className="h-8 rounded-md border border-[var(--border)] px-3 text-xs text-[var(--text-muted)] hover:border-[var(--accent)] hover:text-[var(--accent)] sm:border-0 sm:px-0"
              >
                清除
              </button>
            )}
          </div>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-[var(--border)] bg-[var(--bg-pane)]">
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">时间</th>
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">任务 ID</th>
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">平台</th>
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">状态</th>
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">进度</th>
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">成功/失败</th>
                <th className="px-4 py-2.5 text-left text-xs font-medium text-[var(--text-muted)]">错误</th>
              </tr>
            </thead>
            <tbody>
              {tasks.length === 0 && (
                <tr>
                  <td colSpan={7} className="px-4 py-12 text-center text-sm text-[var(--text-muted)]">
                    暂无任务记录
                  </td>
                </tr>
              )}
              {tasks.map((task) => {
                const success = task.success || 0
                const errorCount = task.error_count || 0
                const total = success + errorCount
                const errorText = formatError(task.error)
                return (
                  <tr
                    key={task.id}
                    className="border-b border-[var(--border)]/50 transition-colors hover:bg-[var(--bg-hover)]"
                  >
                    <td className="whitespace-nowrap px-4 py-3 text-xs text-[var(--text-muted)]">
                      {task.created_at
                        ? new Date(task.created_at).toLocaleString('zh-CN', {
                            month: '2-digit',
                            day: '2-digit',
                            hour: '2-digit',
                            minute: '2-digit',
                            hour12: false,
                          })
                        : '-'}
                    </td>
                    <td className="px-4 py-3">
                      <span
                        className="cursor-default font-mono text-xs text-[var(--text-muted)]"
                        title={task.id}
                      >
                        {shortId(task.id)}
                      </span>
                    </td>
                    <td className="px-4 py-3">
                      <Badge variant="secondary">{task.platform || '-'}</Badge>
                    </td>
                    <td className="px-4 py-3">
                      <Badge variant={TASK_STATUS_VARIANTS[task.status] || 'secondary'}>
                        {getTaskStatusText(task.status)}
                      </Badge>
                    </td>
                    <td className="px-4 py-3 text-xs text-[var(--text-secondary)]">
                      {task.progress || '-'}
                    </td>
                    <td className="px-4 py-3">
                      <div className="flex items-center gap-2">
                        {total > 0 ? (
                          <>
                            <div className="flex h-1.5 w-16 overflow-hidden rounded-full bg-[var(--chip-bg)]">
                              {success > 0 && (
                                <div
                                  className="h-full rounded-full bg-[var(--state-success-strong)]"
                                  style={{ width: `${(success / total) * 100}%` }}
                                />
                              )}
                              {errorCount > 0 && (
                                <div
                                  className="h-full rounded-full bg-[var(--state-danger-strong)]"
                                  style={{ width: `${(errorCount / total) * 100}%` }}
                                />
                              )}
                            </div>
                            <span className="text-xs text-[var(--text-muted)] whitespace-nowrap">
                              <span className="text-[var(--state-success)]">{success}</span>
                              {' / '}
                              <span className="text-[var(--state-danger)]">{errorCount}</span>
                            </span>
                          </>
                        ) : (
                          <span className="text-xs text-[var(--text-muted)]">-</span>
                        )}
                      </div>
                    </td>
                    <td className="max-w-[280px] px-4 py-3">
                      {errorText ? (
                        <span
                          className="block truncate text-xs text-[var(--state-danger)] cursor-default"
                          title={task.error || ''}
                        >
                          {errorText}
                        </span>
                      ) : (
                        <span className="text-xs text-[var(--text-muted)]">-</span>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
