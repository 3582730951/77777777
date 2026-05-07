<template>
  <div class="p-6 space-y-6">
    <h1 class="text-2xl font-bold">Dashboard</h1>

    <!-- KPI Cards -->
    <div class="grid grid-cols-2 md:grid-cols-5 gap-4">
      <div v-for="kpi in kpis" :key="kpi.label" class="bg-white dark:bg-gray-900 rounded-xl p-4 shadow-sm border border-gray-200 dark:border-gray-800">
        <div class="text-sm text-gray-500">{{ kpi.label }}</div>
        <div class="text-2xl font-bold mt-1">{{ kpi.value }}</div>
      </div>
    </div>

    <!-- Charts Row -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-6">
      <div class="bg-white dark:bg-gray-900 rounded-xl p-4 shadow-sm border border-gray-200 dark:border-gray-800">
        <h3 class="font-semibold mb-3">Request Trend</h3>
        <canvas ref="requestChart" height="200"></canvas>
      </div>
      <div class="bg-white dark:bg-gray-900 rounded-xl p-4 shadow-sm border border-gray-200 dark:border-gray-800">
        <h3 class="font-semibold mb-3">Cache Hit Rate</h3>
        <canvas ref="cacheChart" height="200"></canvas>
      </div>
    </div>

    <!-- Account Pool -->
    <div class="bg-white dark:bg-gray-900 rounded-xl p-4 shadow-sm border border-gray-200 dark:border-gray-800">
      <h3 class="font-semibold mb-3">Account Pool</h3>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-3">
        <div v-for="acc in sortedAccounts" :key="accountId(acc)"
          class="p-3 rounded-lg border border-gray-200 dark:border-gray-700 hover:shadow-md transition">
          <div class="flex items-center justify-between">
            <span class="font-mono text-sm truncate">{{ accountId(acc) }}</span>
            <span :class="statusClass(accountStatusCategory(acc))" class="text-xs px-2 py-0.5 rounded-full">{{ accountStatusLabel(acc) }}</span>
          </div>
          <div class="mt-2 text-xs text-gray-500 space-y-1">
            <div>Provider: <span class="font-medium">{{ accountProvider(acc) }}</span></div>
            <div>Latency: <span class="font-medium">{{ formatLatency(acc) }}</span></div>
            <div v-if="quotaRemaining(acc, 'short') !== null">
              5h: {{ quotaRemaining(acc, 'short')?.toFixed(0) }}% left
              <div class="w-full bg-gray-200 dark:bg-gray-700 rounded-full h-1.5 mt-0.5">
                <div class="bg-primary rounded-full h-1.5" :style="{ width: quotaUsedPct(acc, 'short') + '%' }"></div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { accountsAPI, chartsAPI } from '../api'
import { Chart, registerables } from 'chart.js'

Chart.register(...registerables)

const accounts = ref<any[]>([])
const kpis = ref([
  { label: 'Accounts', value: '-' },
  { label: 'Healthy', value: '-' },
  { label: 'Requests/h', value: '-' },
  { label: 'Cache Hit', value: '-' },
  { label: 'Avg Latency', value: '-' },
])

const requestChart = ref<HTMLCanvasElement>()
const cacheChart = ref<HTMLCanvasElement>()

const sortedAccounts = computed(() => [...accounts.value].sort((a, b) => {
  const rankDiff = accountSortRank(a) - accountSortRank(b)
  if (rankDiff !== 0) return rankDiff
  const costDiff = accountPickCost(a) - accountPickCost(b)
  if (costDiff !== 0) return costDiff
  return accountId(a).localeCompare(accountId(b))
}))

function formatNum(n: number): string {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(n >= 10000 ? 0 : 1) + 'K'
  return String(n)
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

function accountStatusCategory(acc: any): string {
  const category = String(accField(acc, 'StatusCategory', 'status_category') || '')
  if (category) return category
  if (accountState(acc) === 'banned') return 'banned'
  if (accountConfidence(acc) === 'probably_exhausted') return 'no_quota'
  if (accountConfidence(acc) === 'cooling' || accountConfidence(acc) === 'suspected_issue') return 'abnormal'
  const shortRemaining = quotaRemaining(acc, 'short')
  const longRemaining = quotaRemaining(acc, 'long')
  if (shortRemaining === 0 || longRemaining === 0) return 'no_quota'
  if ((shortRemaining !== null && shortRemaining <= 10) || (longRemaining !== null && longRemaining <= 10)) return 'low_quota'
  const healthy = accField<boolean>(acc, 'Healthy', 'healthy')
  if (typeof healthy === 'boolean') return healthy ? 'healthy' : 'abnormal'
  return 'healthy'
}

function accountStatusLabel(acc: any): string {
  const label = String(accField(acc, 'StatusLabel', 'status_label') || '')
  if (label) return label
  const map: Record<string, string> = {
    healthy: '健康的',
    low_quota: '额度低',
    no_quota: '没有额度',
    banned: '账号被封禁的',
    abnormal: '账号异常的',
  }
  return map[accountStatusCategory(acc)] || '账号异常的'
}

function accountSortRank(acc: any): number {
  const rank = Number(accField(acc, 'SortRank', 'sort_rank'))
  if (Number.isFinite(rank)) return rank
  const fallback: Record<string, number> = { healthy: 0, low_quota: 1, abnormal: 2, no_quota: 3, banned: 4 }
  return fallback[accountStatusCategory(acc)] ?? 5
}

function accountPickCost(acc: any): number {
  const cost = Number(accField(acc, 'PickCost', 'pick_cost'))
  return Number.isFinite(cost) ? cost : 0
}

function statusClass(category: string) {
  const map: Record<string, string> = {
    healthy: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-300',
    low_quota: 'bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300',
    no_quota: 'bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-300',
    banned: 'bg-rose-100 text-rose-800 dark:bg-rose-900/30 dark:text-rose-300',
    abnormal: 'bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300',
  }
  return map[category] || map.abnormal
}

function quotaRemaining(acc: any, window: 'short' | 'long'): number | null {
  const limit = numberField(acc, window === 'short' ? 'QuotaShortLimit' : 'QuotaLongLimit', window === 'short' ? 'quota_short_limit' : 'quota_long_limit')
  const used = numberField(acc, window === 'short' ? 'QuotaShortUsed' : 'QuotaLongUsed', window === 'short' ? 'quota_short_used' : 'quota_long_used')
  if (limit <= 0) return null
  return limit - used
}

function quotaUsedPct(acc: any, window: 'short' | 'long'): number {
  const limit = numberField(acc, window === 'short' ? 'QuotaShortLimit' : 'QuotaLongLimit', window === 'short' ? 'quota_short_limit' : 'quota_long_limit')
  const used = numberField(acc, window === 'short' ? 'QuotaShortUsed' : 'QuotaLongUsed', window === 'short' ? 'quota_short_used' : 'quota_long_used')
  if (limit <= 0) return 0
  return Math.max(0, Math.min(100, (used * 100) / limit))
}

function isHealthy(acc: any): boolean {
  const category = String(accField(acc, 'StatusCategory', 'status_category') || '')
  if (category) return category === 'healthy'
  const healthy = accField<boolean>(acc, 'Healthy', 'healthy')
  if (typeof healthy === 'boolean') return healthy
  const shortRemaining = quotaRemaining(acc, 'short')
  const longRemaining = quotaRemaining(acc, 'long')
  return numberField(acc, 'BreakerState', 'breaker_state') === 0 && shortRemaining !== 0 && longRemaining !== 0
}

onMounted(async () => {
  try {
    const accs = await accountsAPI.list()
    accounts.value = accs || []
    const healthy = accounts.value.filter(isHealthy).length
    const avgLat = accounts.value.length ? (accounts.value.reduce((s: number, a: any) => s + numberField(a, 'EWMALatency', 'ewma_latency', 'EWMAMs'), 0) / accounts.value.length) : 0
    kpis.value[0].value = String(accounts.value.length)
    kpis.value[1].value = String(healthy)
    kpis.value[4].value = avgLat.toFixed(0) + 'ms'

    const cache = await chartsAPI.cacheHit()
    if (cache) {
      const ratio = cache.Total > 0 ? ((cache.Hits / cache.Total) * 100).toFixed(1) : '0'
      kpis.value[3].value = ratio + '%'
    }

    const series = await chartsAPI.requests()
    if (series) {
      const totalReqs = series.reduce((s: number, b: any) => s + (b.Count || 0), 0)
      kpis.value[2].value = formatNum(totalReqs)
    }
    if (series && requestChart.value) {
      new Chart(requestChart.value, {
        type: 'line',
        data: {
          labels: series.map((s: any) => new Date(s.Bucket).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })),
          datasets: [{
            label: 'Requests',
            data: series.map((s: any) => s.Count),
            borderColor: '#6366f1',
            fill: true,
            backgroundColor: 'rgba(99,102,241,0.1)',
            tension: 0.3,
            pointRadius: series.length > 30 ? 0 : 3,
          }],
        },
        options: {
          responsive: true,
          plugins: { legend: { display: false } },
          scales: {
            x: { ticks: { maxTicksLimit: 12, maxRotation: 0 } },
            y: {
              beginAtZero: true,
              ticks: {
                callback: (v: any) => {
                  if (v >= 1000000) return (v / 1000000).toFixed(1) + 'M'
                  if (v >= 1000) return (v / 1000).toFixed(v >= 10000 ? 0 : 1) + 'K'
                  return v
                },
              },
            },
          },
        },
      })
    }

    const cacheSeries = await chartsAPI.cacheHitSeries()
    if (cacheSeries && cacheChart.value) {
      new Chart(cacheChart.value, {
        type: 'line',
        data: {
          labels: cacheSeries.map((s: any) => new Date(s.bucket).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })),
          datasets: [{
            label: 'Hit %',
            data: cacheSeries.map((s: any) => s.total > 0 ? (s.hits / s.total * 100) : 0),
            borderColor: '#10b981',
            fill: true,
            backgroundColor: 'rgba(16,185,129,0.1)',
            tension: 0.3,
            pointRadius: cacheSeries.length > 30 ? 0 : 3,
          }],
        },
        options: {
          responsive: true,
          plugins: { legend: { display: false } },
          scales: {
            x: { ticks: { maxTicksLimit: 12, maxRotation: 0 } },
            y: { min: 0, max: 100, ticks: { callback: (v: any) => v + '%' } },
          },
        },
      })
    }
  } catch (e) {
    console.error('Dashboard load failed:', e)
  }
})
</script>
