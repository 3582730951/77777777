<template>
  <div class="p-6 space-y-4">
    <h1 class="text-2xl font-bold">Audit Log</h1>
    <div class="overflow-x-auto bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
      <table class="w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800/50">
          <tr>
            <th class="px-4 py-3 text-left font-medium">Time</th>
            <th class="px-4 py-3 text-left font-medium">Level</th>
            <th class="px-4 py-3 text-left font-medium">Category</th>
            <th class="px-4 py-3 text-left font-medium">Account</th>
            <th class="px-4 py-3 text-left font-medium">Message</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
          <tr v-for="e in entries" :key="e.ID" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5 text-xs whitespace-nowrap">{{ new Date(e.At).toLocaleString() }}</td>
            <td class="px-4 py-2.5">
              <span :class="levelClass(e.Level)" class="px-2 py-0.5 rounded-full text-xs">{{ e.Level }}</span>
            </td>
            <td class="px-4 py-2.5">{{ e.Category }}</td>
            <td class="px-4 py-2.5 font-mono text-xs">{{ e.AccountID || '-' }}</td>
            <td class="px-4 py-2.5 text-xs max-w-md truncate">{{ e.Message }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { auditAPI } from '../api'

const entries = ref<any[]>([])

function levelClass(l: string) {
  if (l === 'error') return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (l === 'warn') return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300'
}

onMounted(async () => { entries.value = (await auditAPI.list(100)) || [] })
</script>
