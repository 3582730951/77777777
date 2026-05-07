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
        <div v-for="acc in accounts" :key="acc.AccountID"
          class="p-3 rounded-lg border border-gray-200 dark:border-gray-700 hover:shadow-md transition">
          <div class="flex items-center justify-between">
            <span class="font-mono text-sm truncate">{{ acc.AccountID }}</span>
            <span :class="stateClass(acc)" class="text-xs px-2 py-0.5 rounded-full">{{ acc.Confidence }}</span>
          </div>
          <div class="mt-2 text-xs text-gray-500 space-y-1">
            <div>Provider: <span class="font-medium">{{ acc.Provider }}</span></div>
            <div>Latency: <span class="font-medium">{{ acc.EWMALatency.toFixed(0) }}ms</span></div>
            <div v-if="acc.QuotaShortLimit">
              5h: {{ (acc.QuotaShortLimit - acc.QuotaShortUsed).toFixed(0) }}% left
              <div class="w-full bg-gray-200 dark:bg-gray-700 rounded-full h-1.5 mt-0.5">
                <div class="bg-primary rounded-full h-1.5" :style="{ width: acc.QuotaShortUsed + '%' }"></div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
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

function formatNum(n: number): string {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(n >= 10000 ? 0 : 1) + 'K'
  return String(n)
}

function stateClass(acc: any) {
  if (!isHealthy(acc)) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (acc.BreakerState > 0) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (acc.Confidence === 'confirmed_available') return 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400'
  return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
}

function quotaRemaining(acc: any, window: 'short' | 'long'): number | null {
  const limit = Number(acc[window === 'short' ? 'QuotaShortLimit' : 'QuotaLongLimit'] ?? acc[window === 'short' ? 'quota_short_limit' : 'quota_long_limit'] ?? 0)
  const used = Number(acc[window === 'short' ? 'QuotaShortUsed' : 'QuotaLongUsed'] ?? acc[window === 'short' ? 'quota_short_used' : 'quota_long_used'] ?? 0)
  if (limit <= 0) return null
  return limit - used
}

function isHealthy(acc: any): boolean {
  if (typeof acc.Healthy === 'boolean') return acc.Healthy
  if (typeof acc.healthy === 'boolean') return acc.healthy
  const shortRemaining = quotaRemaining(acc, 'short')
  const longRemaining = quotaRemaining(acc, 'long')
  return acc.BreakerState === 0 && shortRemaining !== 0 && longRemaining !== 0
}

onMounted(async () => {
  try {
    const accs = await accountsAPI.list()
    accounts.value = accs || []
    const healthy = accounts.value.filter(isHealthy).length
    const avgLat = accounts.value.length ? (accounts.value.reduce((s: number, a: any) => s + a.EWMALatency, 0) / accounts.value.length) : 0
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
