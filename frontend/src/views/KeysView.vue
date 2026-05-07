<template>
  <div class="p-6 space-y-4">
    <div class="flex items-center justify-between">
      <h1 class="text-2xl font-bold">API Keys</h1>
      <button @click="showCreate = true" class="px-3 py-1.5 bg-primary text-white rounded-lg text-sm">+ New Key</button>
    </div>
    <div class="overflow-x-auto bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
      <table class="w-full text-sm">
        <thead class="bg-gray-50 dark:bg-gray-800/50">
          <tr>
            <th class="px-4 py-3 text-left font-medium">Key</th>
            <th class="px-4 py-3 text-left font-medium">Label</th>
            <th class="px-4 py-3 text-left font-medium">Tenant</th>
            <th class="px-4 py-3 text-left font-medium">Group</th>
            <th class="px-4 py-3 text-left font-medium">Created</th>
            <th class="px-4 py-3 text-center font-medium">Actions</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-gray-800">
          <tr v-for="k in keys" :key="k.Value" class="hover:bg-gray-50 dark:hover:bg-gray-800/30">
            <td class="px-4 py-2.5 font-mono text-xs">{{ k.Value.slice(0, 20) }}...</td>
            <td class="px-4 py-2.5">{{ k.Label }}</td>
            <td class="px-4 py-2.5">{{ k.TenantID }}</td>
            <td class="px-4 py-2.5">{{ k.GroupID }}</td>
            <td class="px-4 py-2.5">{{ new Date(k.CreatedAt).toLocaleDateString() }}</td>
            <td class="px-4 py-2.5 text-center">
              <button v-if="!k.RevokedAt" @click="revoke(k.Value)" class="text-xs text-red-500 hover:underline">Revoke</button>
              <span v-else class="text-xs text-gray-400">Revoked</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { keysAPI } from '../api'

const keys = ref<any[]>([])
const showCreate = ref(false)

async function refresh() { keys.value = (await keysAPI.list()) || [] }
async function revoke(v: string) {
  if (confirm('Revoke this key?')) {
    await keysAPI.revoke(v)
    refresh()
  }
}

onMounted(refresh)
</script>
