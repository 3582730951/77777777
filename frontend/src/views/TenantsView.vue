<template>
  <div class="p-6 space-y-4">
    <h1 class="text-2xl font-bold">Tenants</h1>
    <div class="overflow-x-auto bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
      <table class="w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800/50">
          <tr>
            <th class="px-4 py-3 text-left font-medium">ID</th>
            <th class="px-4 py-3 text-left font-medium">Name</th>
            <th class="px-4 py-3 text-left font-medium">Created</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
          <tr v-for="t in tenants" :key="t.ID" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5 font-mono text-xs">{{ t.ID }}</td>
            <td class="px-4 py-2.5">{{ t.Name }}</td>
            <td class="px-4 py-2.5">{{ new Date(t.CreatedAt).toLocaleDateString() }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { tenantsAPI } from '../api'
const tenants = ref<any[]>([])
onMounted(async () => { tenants.value = (await tenantsAPI.list()) || [] })
</script>
