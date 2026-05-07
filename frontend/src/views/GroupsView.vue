<template>
  <div class="p-6 space-y-4">
    <div class="flex items-center justify-between">
      <h1 class="text-2xl font-bold">Groups</h1>
    </div>
    <div class="overflow-x-auto bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
      <table class="w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800/50">
          <tr>
            <th class="px-4 py-3 text-left font-medium">ID</th>
            <th class="px-4 py-3 text-left font-medium">Tenant</th>
            <th class="px-4 py-3 text-left font-medium">Provider</th>
            <th class="px-4 py-3 text-left font-medium">Models</th>
            <th class="px-4 py-3 text-left font-medium">Reasoning</th>
            <th class="px-4 py-3 text-left font-medium">Forced Model</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
          <tr v-for="g in groups" :key="g.ID" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5 font-mono text-xs">{{ g.ID }}</td>
            <td class="px-4 py-2.5">{{ g.TenantID }}</td>
            <td class="px-4 py-2.5">{{ g.Provider }}</td>
            <td class="px-4 py-2.5">
              <span v-for="m in (g.Models || []).slice(0,3)" :key="m" class="inline-block mr-1 px-1.5 py-0.5 bg-gray-100 dark:bg-gray-800 rounded text-xs">{{ m }}</span>
            </td>
            <td class="px-4 py-2.5">{{ g.ReasoningEffort || '-' }}</td>
            <td class="px-4 py-2.5">{{ g.ForcedModel || '-' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { groupsAPI } from '../api'

const groups = ref<any[]>([])
onMounted(async () => { groups.value = (await groupsAPI.list()) || [] })
</script>
