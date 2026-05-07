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
          <tr v-for="acc in filtered" :key="acc.AccountID" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5 font-mono text-xs">{{ acc.AccountID }}</td>
            <td class="px-4 py-2.5">
              <span :class="providerClass(acc.Provider)" class="px-2 py-0.5 rounded-full text-xs font-medium">{{ acc.Provider }}</span>
            </td>
            <td class="px-4 py-2.5">{{ acc.State }}</td>
            <td class="px-4 py-2.5">{{ acc.Confidence }}</td>
            <td class="px-4 py-2.5 text-right">{{ acc.EWMALatency.toFixed(0) }}ms</td>
            <td class="px-4 py-2.5 text-right">
              <span v-if="acc.QuotaShortLimit">{{ (acc.QuotaShortLimit - acc.QuotaShortUsed).toFixed(0) }}%</span>
              <span v-else class="text-gray-400">-</span>
            </td>
            <td class="px-4 py-2.5 text-right">
              <span v-if="acc.QuotaLongLimit">{{ (acc.QuotaLongLimit - acc.QuotaLongUsed).toFixed(0) }}%</span>
              <span v-else class="text-gray-400">-</span>
            </td>
            <td class="px-4 py-2.5 text-center space-x-1">
              <button @click="probe(acc.AccountID)" class="text-xs text-primary hover:underline">Probe</button>
              <button @click="discover(acc.AccountID)" class="text-xs text-primary hover:underline">Discover</button>
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

const filtered = computed(() => {
  if (!filterProvider.value) return accounts.value
  return accounts.value.filter(a => a.Provider === filterProvider.value)
})

function providerClass(p: string) {
  const map: Record<string, string> = {
    claude: 'bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-400',
    chatgpt: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-400',
    gemini: 'bg-blue-100 text-blue-800 dark:bg-blue-900/30 dark:text-blue-400',
  }
  return map[p] || 'bg-gray-100 text-gray-700'
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
