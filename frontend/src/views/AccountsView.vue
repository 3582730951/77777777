<template>
  <div class="p-6 space-y-4">
    <div class="flex items-center justify-between">
      <h1 class="text-2xl font-bold">Accounts</h1>
      <div class="flex gap-2">
        <select v-model="filterProvider" class="px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-700 bg-transparent text-sm">
          <option value="">All Providers</option>
          <option value="claude">Claude</option>
          <option value="chatgpt">ChatGPT</option>
          <option value="gemini">Gemini</option>
          <option value="cursor">Cursor</option>
          <option value="kiro">Kiro</option>
          <option value="windsurf">Windsurf</option>
          <option value="grok">Grok</option>
          <option value="trae">Trae</option>
          <option value="blink">Blink</option>
          <option value="cerebras">Cerebras</option>
          <option value="tavily">Tavily</option>
          <option value="openblocklabs">OpenBlockLabs</option>
        </select>
        <button @click="refresh" class="px-3 py-1.5 bg-primary text-white rounded-lg text-sm">Refresh</button>
      </div>
    </div>

    <div class="overflow-x-auto bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
      <table class="w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800/50">
          <tr>
            <th class="px-4 py-3 text-left font-medium">ID</th>
            <th class="px-4 py-3 text-left font-medium">Provider</th>
            <th class="px-4 py-3 text-left font-medium">State</th>
            <th class="px-4 py-3 text-left font-medium">Confidence</th>
            <th class="px-4 py-3 text-right font-medium">Latency</th>
            <th class="px-4 py-3 text-right font-medium">5h Quota</th>
            <th class="px-4 py-3 text-right font-medium">7d Quota</th>
            <th class="px-4 py-3 text-center font-medium">Actions</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
          <tr v-for="acc in filtered" :key="accountId(acc)" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5">
              <div class="flex items-center gap-2">
                <span class="font-mono text-xs">{{ accountId(acc) }}</span>
                <button
                  type="button"
                  class="inline-flex h-7 w-7 items-center justify-center rounded-md border border-gray-200 text-gray-500 hover:bg-gray-100 hover:text-gray-900 dark:border-gray-700 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-gray-100"
                  :aria-label="emailVisible(acc) ? '隐藏注册邮箱' : '显示注册邮箱'"
                  :title="emailVisible(acc) ? '隐藏注册邮箱' : '显示注册邮箱'"
                  @click="toggleEmail(acc)"
                >
                  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" class="h-4 w-4">
                    <path d="M2.1 12s3.6-6.5 9.9-6.5 9.9 6.5 9.9 6.5-3.6 6.5-9.9 6.5S2.1 12 2.1 12Z" />
                    <circle cx="12" cy="12" r="2.8" />
                  </svg>
                </button>
              </div>
              <div v-if="emailVisible(acc)" class="mt-1 max-w-[260px] break-all font-mono text-[11px] text-gray-500 dark:text-gray-400">
                {{ accountEmail(acc) || '未记录邮箱' }}
              </div>
            </td>
            <td class="px-4 py-2.5">
              <span :class="providerClass(accountProvider(acc))" class="px-2 py-0.5 rounded-full text-xs font-medium">{{ accountProvider(acc) }}</span>
            </td>
            <td class="px-4 py-2.5">{{ accountState(acc) }}</td>
            <td class="px-4 py-2.5">{{ accountConfidence(acc) }}</td>
            <td class="px-4 py-2.5 text-right">{{ formatLatency(acc) }}</td>
            <td class="px-4 py-2.5 text-right">
              <span v-if="formatQuotaRemaining(acc, 'short')">{{ formatQuotaRemaining(acc, 'short') }}</span>
              <span v-else class="text-gray-400">-</span>
            </td>
            <td class="px-4 py-2.5 text-right">
              <span v-if="formatQuotaRemaining(acc, 'long')">{{ formatQuotaRemaining(acc, 'long') }}</span>
              <span v-else class="text-gray-400">-</span>
            </td>
            <td class="px-4 py-2.5 text-center space-x-1">
              <button @click="probe(accountId(acc))" class="text-xs text-primary hover:underline">Probe</button>
              <button @click="discover(accountId(acc))" class="text-xs text-primary hover:underline">Discover</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { accountsAPI } from '../api'

const accounts = ref<any[]>([])
const filterProvider = ref('')
const visibleEmails = ref<Record<string, boolean>>({})

const filtered = computed(() => {
  const list = !filterProvider.value
    ? accounts.value
    : accounts.value.filter(a => accountProvider(a) === filterProvider.value)
  return [...list].sort((a, b) => {
    if (isHealthy(a) !== isHealthy(b)) return isHealthy(a) ? -1 : 1
    return accountId(a).localeCompare(accountId(b))
  })
})

function providerClass(p: string) {
  const map: Record<string, string> = {
    claude: 'bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-400',
    chatgpt: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-400',
    gemini: 'bg-blue-100 text-blue-800 dark:bg-blue-900/30 dark:text-blue-400',
  }
  return map[p] || 'bg-gray-100 text-gray-700'
}

function accField<T = any>(acc: any, ...keys: string[]): T | undefined {
  for (const key of keys) {
    if (acc?.[key] !== undefined && acc[key] !== null) return acc[key] as T
  }
  return undefined
}

function accountId(acc: any): string {
  return String(accField(acc, 'AccountID', 'account_id', 'ID', 'id') || '')
}

function accountProvider(acc: any): string {
  return String(accField(acc, 'Provider', 'provider') || '')
}

function accountEmail(acc: any): string {
  return String(accField(acc, 'Email', 'email') || '')
}

function accountState(acc: any): string {
  return String(accField(acc, 'State', 'state') || '-')
}

function accountConfidence(acc: any): string {
  return String(accField(acc, 'Confidence', 'confidence') || '-')
}

function numberField(acc: any, ...keys: string[]): number {
  const n = Number(accField(acc, ...keys) ?? 0)
  return Number.isFinite(n) ? n : 0
}

function formatLatency(acc: any): string {
  return `${numberField(acc, 'EWMALatency', 'ewma_latency', 'EWMAMs').toFixed(0)}ms`
}

function quotaRemaining(acc: any, window: 'short' | 'long'): number | null {
  const limit = numberField(acc, window === 'short' ? 'QuotaShortLimit' : 'QuotaLongLimit', window === 'short' ? 'quota_short_limit' : 'quota_long_limit')
  const used = numberField(acc, window === 'short' ? 'QuotaShortUsed' : 'QuotaLongUsed', window === 'short' ? 'quota_short_used' : 'quota_long_used')
  if (limit <= 0) return null
  return Math.max(0, limit - used)
}

function formatQuotaRemaining(acc: any, window: 'short' | 'long'): string {
  const remaining = quotaRemaining(acc, window)
  return remaining === null ? '' : `${remaining.toFixed(0)}%`
}

function isHealthy(acc: any): boolean {
  const healthy = accField<boolean>(acc, 'Healthy', 'healthy')
  if (typeof healthy === 'boolean') return healthy
  if (['banned', 'disabled'].includes(accountState(acc))) return false
  if (Boolean(accField(acc, 'BreakerOpen', 'breaker_open')) || numberField(acc, 'BreakerState', 'breaker_state') > 0) return false
  return ['confirmed_available', 'likely_available'].includes(accountConfidence(acc)) || accountState(acc) === 'active'
}

function emailVisible(acc: any): boolean {
  return visibleEmails.value[accountId(acc)] === true
}

function toggleEmail(acc: any) {
  const id = accountId(acc)
  visibleEmails.value[id] = !visibleEmails.value[id]
}

async function refresh() {
  accounts.value = (await accountsAPI.list()) || []
}

async function probe(id: string) {
  await accountsAPI.probe(id)
  refresh()
}

async function discover(id: string) {
  await accountsAPI.discover(id)
  refresh()
}

onMounted(refresh)
</script>
