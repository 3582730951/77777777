import { useState } from 'react'
import { apiFetch } from '@/lib/utils'
import type { ProviderOption, ProviderSetting } from '@/lib/config-options'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Dialog } from '@/components/ui/dialog'
import { Save, Eye, EyeOff, X, Pencil, Plus, Trash2, FlaskConical } from 'lucide-react'
import { invalidateConfigOptionsCache } from '@/lib/app-data'

const CATEGORY_GROUPS = [
  { key: 'free', label: '免费 / 开箱即用', desc: '无需自建服务，直接使用' },
  { key: 'selfhost', label: '需要自建服务', desc: '需要自行部署后端服务' },
  { key: 'thirdparty', label: '第三方服务', desc: '需要注册第三方平台获取凭据' },
  { key: 'custom', label: '自定义', desc: '通过通用 HTTP 驱动对接任意 API' },
]

/* ------------------------------------------------------------------ */
/*  Toggle                                                             */
/* ------------------------------------------------------------------ */
function Toggle({ checked, onChange, disabled, label }: { checked: boolean; onChange: (v: boolean) => void; disabled?: boolean; label: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors ${
        disabled ? 'opacity-40 cursor-not-allowed' : 'cursor-pointer'
      } ${checked ? 'bg-[var(--accent)]' : 'bg-[var(--chip-bg)] border border-[var(--border)]'}`}
    >
      <span className={`inline-block h-3.5 w-3.5 rounded-full bg-white shadow transition-transform ${checked ? 'translate-x-[18px]' : 'translate-x-[3px]'}`} />
    </button>
  )
}

/* ------------------------------------------------------------------ */
/*  Edit modal                                                         */
/* ------------------------------------------------------------------ */
function EditModal({
  provider, setting, providerType, onClose, onSaved,
}: {
  provider: ProviderOption; setting: ProviderSetting | null; providerType: string
  onClose: () => void; onSaved: () => void
}) {
  const fields = provider.fields || []
  const [form, setForm] = useState<Record<string, string>>(() => {
    const data: Record<string, string> = {}
    for (const field of fields) {
      data[field.key] = (setting?.auth?.[field.key] || '') || (setting?.config?.[field.key] || '')
    }
    return data
  })
  const [showSecret, setShowSecret] = useState<Record<string, boolean>>({})
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; message?: string; error?: string } | null>(null)

  const handleSave = async () => {
    const config: Record<string, string> = {}
    const auth: Record<string, string> = {}
    for (const field of fields) {
      if (field.category === 'auth') auth[field.key] = form[field.key] || ''
      else config[field.key] = form[field.key] || ''
    }
    setSaving(true)
    try {
      if (setting) {
        await apiFetch('/provider-settings', {
          method: 'PUT',
          body: JSON.stringify({
            id: setting.id, provider_type: providerType, provider_key: provider.value,
            display_name: setting.display_name || provider.label,
            auth_mode: setting.auth_mode || provider.default_auth_mode || '',
            enabled: true, is_default: setting.is_default, config, auth, metadata: {},
          }),
        })
      } else {
        await apiFetch('/provider-settings', {
          method: 'POST',
          body: JSON.stringify({
            provider_type: providerType, provider_key: provider.value,
            display_name: provider.label, auth_mode: provider.default_auth_mode || '',
            enabled: true, is_default: false, config, auth, metadata: {},
          }),
        })
      }
      invalidateConfigOptionsCache()
      setSaved(true)
      setTimeout(() => { onSaved(); onClose() }, 500)
    } catch (e) { console.error(e) } finally { setSaving(false) }
  }

  const handleTest = async () => {
    const config: Record<string, string> = {}
    const auth: Record<string, string> = {}
    for (const field of fields) {
      if (field.category === 'auth') auth[field.key] = form[field.key] || ''
      else config[field.key] = form[field.key] || ''
    }
    setTesting(true)
    setTestResult(null)
    try {
      const result = await apiFetch('/provider-settings/test', {
        method: 'POST',
        body: JSON.stringify({
          provider_type: providerType,
          provider_key: provider.value,
          config, auth,
        }),
      })
      setTestResult(result)
    } catch (e: any) {
      setTestResult({ ok: false, error: e.message || '测试请求失败' })
    } finally {
      setTesting(false)
    }
  }

  return (
    <Dialog titleId="provider-edit-title" onClose={onClose} className="dialog-panel-sm flex flex-col">
        <div className="flex items-center justify-between border-b border-[var(--border)] px-5 py-4">
          <div>
            <h2 id="provider-edit-title" className="text-base font-semibold text-[var(--text-primary)]">{provider.label}</h2>
            {provider.description && <p className="mt-0.5 text-xs text-[var(--text-muted)]">{provider.description}</p>}
          </div>
          <button type="button" aria-label="关闭 Provider 编辑弹窗" onClick={onClose} className="text-[var(--text-muted)] hover:text-[var(--text-primary)]"><X className="h-4 w-4" /></button>
        </div>
        <div className="flex-1 overflow-y-auto px-5 py-4 space-y-4">
          {fields.length === 0 ? (
            <p className="text-sm text-[var(--text-muted)]">此服务无需额外配置。</p>
          ) : fields.map(field => {
            const sk = `${provider.value}:${field.key}`
            const fieldId = `provider-edit-${sk.replace(/[^a-zA-Z0-9_-]/g, '-')}`
            return (
              <div key={field.key}>
                <label htmlFor={fieldId} className="mb-1.5 block text-sm font-medium text-[var(--text-secondary)]">{field.label}</label>
                <div className="relative">
                  {field.type === 'select' && field.options?.length ? (
                    <select id={fieldId} value={form[field.key] || ''} onChange={e => setForm(f => ({ ...f, [field.key]: e.target.value }))} className="control-surface appearance-none">
                      {field.options.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                    </select>
                  ) : (
                    <>
                      <input id={fieldId} type={field.secret && !showSecret[sk] ? 'password' : 'text'} value={form[field.key] || ''}
                        onChange={e => setForm(f => ({ ...f, [field.key]: e.target.value }))}
                        placeholder={field.placeholder || ''} className="control-surface pr-9" autoComplete="new-password"
                        data-1p-ignore data-lpignore="true" />
                      {field.secret && (
                        <button type="button"
                          aria-label={showSecret[sk] ? `隐藏${field.label}` : `显示${field.label}`}
                          onClick={() => setShowSecret(s => ({ ...s, [sk]: !s[sk] }))}
                          className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--text-muted)] hover:text-[var(--text-secondary)]">
                          {showSecret[sk] ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                        </button>
                      )}
                    </>
                  )}
                </div>
              </div>
            )
          })}
        </div>
        {/* Test result */}
        {testResult && (
          <div className={`mx-5 rounded-lg px-3 py-2 text-xs ${
            testResult.ok
              ? 'border border-[var(--badge-success-border)] bg-[var(--badge-success-bg)] text-[var(--badge-success-fg)]'
              : 'border border-[var(--badge-danger-border)] bg-[var(--badge-danger-bg)] text-[var(--badge-danger-fg)]'
          }`}>
            {testResult.ok ? testResult.message : testResult.error}
          </div>
        )}

        {/* Footer */}
        <div className="flex gap-2 border-t border-[var(--border)] px-5 py-3">
          <Button onClick={handleSave} disabled={saving} className="flex-1">
            <Save className="h-3.5 w-3.5 mr-1.5" />
            {saved ? '已保存 ✓' : saving ? '保存中...' : '保存'}
          </Button>
          <Button variant="outline" onClick={handleTest} disabled={testing || fields.length === 0} className="flex-1">
            <FlaskConical className="h-3.5 w-3.5 mr-1.5" />
            {testing ? '测试中...' : '测试连接'}
          </Button>
          <Button variant="outline" onClick={onClose}>取消</Button>
        </div>
    </Dialog>
  )
}

/* ------------------------------------------------------------------ */
/*  Main                                                               */
/* ------------------------------------------------------------------ */
type Props = {
  providerType: string
  catalog: ProviderOption[]
  settings: ProviderSetting[]
  onReload: () => Promise<void>
  onCreateCustom?: () => void
}

export default function ProviderCards({ providerType, catalog, settings, onReload, onCreateCustom }: Props) {
  const [editTarget, setEditTarget] = useState<{ provider: ProviderOption; setting: ProviderSetting | null } | null>(null)
  const [loading, setLoading] = useState<Record<string, boolean>>({})
  const [testResults, setTestResults] = useState<Record<string, { ok: boolean; message?: string; error?: string }>>({})
  const [testingKeys, setTestingKeys] = useState<Record<string, boolean>>({})

  const settingsMap: Record<string, ProviderSetting> = {}
  for (const s of settings) settingsMap[s.provider_key] = s
  const defaultKey = settings.find(s => s.is_default)?.provider_key || ''

  const grouped: Record<string, ProviderOption[]> = {}
  for (const p of catalog) {
    const cat = p.category || 'custom'
    if (!grouped[cat]) grouped[cat] = []
    grouped[cat].push(p)
  }

  const withLoading = async (key: string, fn: () => Promise<void>) => {
    setLoading(p => ({ ...p, [key]: true }))
    try { await fn() } finally { setLoading(p => ({ ...p, [key]: false })) }
  }

  const handleToggle = (provider: ProviderOption, enable: boolean) => withLoading(provider.value, async () => {
    const setting = settingsMap[provider.value]
    if (enable && !setting) {
      await apiFetch('/provider-settings', {
        method: 'POST',
        body: JSON.stringify({
          provider_type: providerType, provider_key: provider.value,
          display_name: provider.label, auth_mode: provider.default_auth_mode || '',
          enabled: true, is_default: settings.length === 0, config: {}, auth: {}, metadata: {},
        }),
      })
    } else if (!enable && setting) {
      await apiFetch(`/provider-settings/${setting.id}`, { method: 'DELETE' })
    }
    invalidateConfigOptionsCache()
    await onReload()
  })

  const handleSetDefault = (provider: ProviderOption) => withLoading(provider.value, async () => {
    const setting = settingsMap[provider.value]
    if (!setting) return
    await apiFetch('/provider-settings', {
      method: 'PUT',
      body: JSON.stringify({
        id: setting.id, provider_type: providerType, provider_key: provider.value,
        display_name: setting.display_name, auth_mode: setting.auth_mode,
        enabled: true, is_default: true, config: setting.config, auth: setting.auth, metadata: {},
      }),
    })
    invalidateConfigOptionsCache()
    await onReload()
  })

  const handleTestInline = async (provider: ProviderOption) => {
    const setting = settingsMap[provider.value]
    if (!setting) return
    const key = provider.value
    setTestingKeys(p => ({ ...p, [key]: true }))
    setTestResults(p => { const n = { ...p }; delete n[key]; return n })
    try {
      const result = await apiFetch('/provider-settings/test', {
        method: 'POST',
        body: JSON.stringify({
          provider_type: providerType,
          provider_key: key,
          config: setting.config || {},
          auth: setting.auth || {},
        }),
      })
      setTestResults(p => ({ ...p, [key]: result }))
    } catch (e: any) {
      setTestResults(p => ({ ...p, [key]: { ok: false, error: e.message || '测试失败' } }))
    } finally {
      setTestingKeys(p => ({ ...p, [key]: false }))
    }
  }

  const handleDelete = (provider: ProviderOption) => withLoading(provider.value, async () => {
    const setting = settingsMap[provider.value]
    if (!setting) return
    // Delete the setting
    await apiFetch(`/provider-settings/${setting.id}`, { method: 'DELETE' })
    // Delete the definition (only works for non-builtin)
    const def = catalog.find(p => p.value === provider.value)
    if (def && !def.is_builtin && (def as any).id) {
      try {
        await apiFetch(`/provider-definitions/${(def as any).id}`, { method: 'DELETE' })
      } catch {
        // definition delete may fail if it's builtin, ignore
      }
    }
    invalidateConfigOptionsCache()
    await onReload()
  })

  const renderCard = (provider: ProviderOption, allowDelete = false) => {
    const key = provider.value
    const setting = settingsMap[key]
    const isEnabled = !!setting
    const isDefault = key === defaultKey
    const hasFields = (provider.fields || []).length > 0

    return (
      <div key={key}>
        <div className="flex flex-col gap-3 rounded-lg border border-[var(--border)] bg-[var(--bg-card)] px-4 py-3 sm:flex-row sm:items-center">
          {/* Left: name + desc + badge */}
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <span className="text-sm font-medium text-[var(--text-primary)]">{provider.label}</span>
              {isDefault && <Badge variant="success">默认</Badge>}
            </div>
            {provider.description && (
              <p className="mt-0.5 text-xs text-[var(--text-muted)] line-clamp-1">{provider.description}</p>
            )}
          </div>

          {/* Right: actions */}
          <div className="grid grid-cols-2 gap-2 sm:flex sm:shrink-0 sm:flex-wrap sm:items-center sm:justify-end">
            <button
              type="button"
              onClick={() => hasFields && isEnabled ? setEditTarget({ provider, setting }) : undefined}
              disabled={!hasFields || !isEnabled}
              className={`table-action-btn justify-center ${(!hasFields || !isEnabled) ? 'opacity-30 cursor-not-allowed' : ''}`}
            >
              <Pencil className="h-3 w-3 mr-1" /> 编辑
            </button>

            <button
              type="button"
              onClick={() => isEnabled ? handleTestInline(provider) : undefined}
              disabled={!isEnabled || testingKeys[key]}
              className={`table-action-btn justify-center ${!isEnabled ? 'opacity-30 cursor-not-allowed' : ''}`}
            >
              <FlaskConical className="h-3 w-3 mr-1" /> {testingKeys[key] ? '测试中' : '测试'}
            </button>

            <button
              type="button"
              onClick={() => isEnabled && !isDefault ? handleSetDefault(provider) : undefined}
              disabled={!isEnabled || isDefault || loading[key]}
              className={`table-action-btn justify-center ${(!isEnabled || isDefault) ? 'opacity-30 cursor-not-allowed' : ''}`}
            >
              {isDefault ? '默认 ✓' : '设默认'}
            </button>

            {allowDelete && (
              <button
                type="button"
                onClick={() => isEnabled ? handleDelete(provider) : undefined}
                disabled={!isEnabled || isDefault || loading[key]}
                className={`table-action-btn table-action-btn-danger justify-center ${(!isEnabled || isDefault) ? 'opacity-30 cursor-not-allowed' : ''}`}
              >
                <Trash2 className="h-3 w-3 mr-1" /> 删除
              </button>
            )}

            <div className="flex items-center justify-center rounded-md border border-[var(--border)] bg-[var(--bg-pane)]/40 px-3 py-1 sm:border-0 sm:bg-transparent sm:px-0 sm:py-0">
              <Toggle
                checked={isEnabled}
                onChange={v => handleToggle(provider, v)}
                disabled={loading[key] || isDefault}
                label={`${isEnabled ? '停用' : '启用'} ${provider.label}`}
              />
            </div>
          </div>
        </div>
        {/* Inline test result */}
        {testResults[key] && (
          <div className={`mt-1 rounded-lg px-3 py-2 text-xs ${
            testResults[key].ok
              ? 'border border-[var(--badge-success-border)] bg-[var(--badge-success-bg)] text-[var(--badge-success-fg)]'
              : 'border border-[var(--badge-danger-border)] bg-[var(--badge-danger-bg)] text-[var(--badge-danger-fg)]'
          }`}>
            {testResults[key].ok ? testResults[key].message : testResults[key].error}
          </div>
        )}
      </div>
    )
  }

  return (
    <>
      <div className="space-y-6">
        {CATEGORY_GROUPS.map(({ key: cat, label, desc }) => {
          const providers = grouped[cat]
          if (!providers || providers.length === 0) return null

          // Hide "通用 HTTP 邮箱" from the list — it's the engine behind custom providers
          const visible = cat === 'custom'
            ? providers.filter(p => p.value !== 'generic_http_mailbox')
            : providers

          return (
            <div key={cat}>
              <div className="mb-2">
                <h3 className="text-sm font-semibold text-[var(--text-primary)]">{label}</h3>
                <p className="text-xs text-[var(--text-muted)]">{desc}</p>
              </div>
              <div className="space-y-1.5">
                {visible.map(p => renderCard(p, cat === 'custom'))}
                {cat === 'custom' && (
                  <button
                    type="button"
                    className="flex w-full items-center justify-center gap-2 rounded-lg border border-dashed border-[var(--border)] px-4 py-3 text-sm text-[var(--text-muted)] transition-colors hover:border-[var(--accent)] hover:text-[var(--accent)]"
                    onClick={() => onCreateCustom?.()}
                  >
                    <Plus className="h-4 w-4" />
                    添加自定义{providerType === 'mailbox' ? '邮箱' : providerType === 'captcha' ? '验证' : providerType === 'sms' ? '接码' : ''}服务
                  </button>
                )}
              </div>
            </div>
          )
        })}
      </div>

      {editTarget && (
        <EditModal
          provider={editTarget.provider}
          setting={editTarget.setting}
          providerType={providerType}
          onClose={() => setEditTarget(null)}
          onSaved={onReload}
        />
      )}
    </>
  )
}
