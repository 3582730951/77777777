<template>
  <div class="p-6 space-y-4">
    <h1 class="text-2xl font-bold">Registration Stats</h1>

    <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
      <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-4">
        <div class="text-sm text-gray-500">Total Accounts</div>
        <div class="text-3xl font-bold mt-1">{{ stats.total || 0 }}</div>
      </div>
      <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-4">
        <div class="text-sm text-gray-500">Active</div>
        <div class="text-3xl font-bold mt-1 text-green-600">{{ stats.active || 0 }}</div>
      </div>
      <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-4">
        <div class="text-sm text-gray-500">Synced to Pool</div>
        <div class="text-3xl font-bold mt-1 text-blue-600">{{ stats.synced || 0 }}</div>
      </div>
    </div>

    <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-4">
      <h2 class="text-lg font-semibold mb-2">By Platform</h2>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-gray-50 dark:bg-gray-800/50">
            <tr>
              <th class="px-4 py-2 text-left">Platform</th>
              <th class="px-4 py-2 text-right">Total</th>
              <th class="px-4 py-2 text-right">Active</th>
              <th class="px-4 py-2 text-right">Banned</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
            <tr v-for="item in stats.by_platform || []" :key="item.platform">
              <td class="px-4 py-2">{{ item.platform }}</td>
              <td class="px-4 py-2 text-right">{{ item.total }}</td>
              <td class="px-4 py-2 text-right text-green-600">{{ item.active }}</td>
              <td class="px-4 py-2 text-right text-red-600">{{ item.banned }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { autoregAPI } from '../../api'

const stats = ref<any>({})

onMounted(async () => {
  try {
    const { data } = await autoregAPI.getStats()
    stats.value = data
  } catch {}
})
</script>
