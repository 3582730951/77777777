<template>
  <div class="p-6 space-y-4">
    <div class="flex items-center justify-between">
      <h1 class="text-2xl font-bold">Registration Tasks</h1>
      <div class="flex gap-2">
        <router-link to="/autoreg/tasks/create" class="px-3 py-1.5 bg-primary text-white rounded-lg text-sm">New Task</router-link>
        <button @click="refresh" class="px-3 py-1.5 border border-gray-300 dark:border-gray-700 rounded-lg text-sm">Refresh</button>
      </div>
    </div>

    <div class="overflow-x-auto bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
      <table class="w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800/50">
          <tr>
            <th class="px-4 py-3 text-left font-medium">ID</th>
            <th class="px-4 py-3 text-left font-medium">Platform</th>
            <th class="px-4 py-3 text-left font-medium">Status</th>
            <th class="px-4 py-3 text-right font-medium">Target</th>
            <th class="px-4 py-3 text-right font-medium">Success</th>
            <th class="px-4 py-3 text-right font-medium">Failed</th>
            <th class="px-4 py-3 text-left font-medium">Created</th>
            <th class="px-4 py-3 text-center font-medium">Actions</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
          <tr v-for="task in tasks" :key="task.id" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5 font-mono text-xs">{{ task.id }}</td>
            <td class="px-4 py-2.5">{{ task.platform }}</td>
            <td class="px-4 py-2.5">
              <span :class="statusClass(task.status)" class="px-2 py-0.5 rounded-full text-xs font-medium">{{ task.status }}</span>
            </td>
            <td class="px-4 py-2.5 text-right">{{ task.target_count }}</td>
            <td class="px-4 py-2.5 text-right text-green-600">{{ task.success_count }}</td>
            <td class="px-4 py-2.5 text-right text-red-600">{{ task.failed_count }}</td>
            <td class="px-4 py-2.5 text-xs">{{ task.created_at }}</td>
            <td class="px-4 py-2.5 text-center">
              <router-link :to="`/autoreg/tasks/${task.id}`" class="text-xs text-primary hover:underline">Logs</router-link>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { autoregAPI } from '../../api'

const tasks = ref<any[]>([])

async function refresh() {
  const { data } = await autoregAPI.listTasks()
  tasks.value = data.items || data || []
}

function statusClass(status: string) {
  switch (status) {
    case 'running': return 'bg-blue-100 text-blue-800 dark:bg-blue-900/30 dark:text-blue-400'
    case 'completed': return 'bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-400'
    case 'failed': return 'bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-400'
    default: return 'bg-gray-100 text-gray-800 dark:bg-gray-900/30 dark:text-gray-400'
  }
}

onMounted(refresh)
</script>
