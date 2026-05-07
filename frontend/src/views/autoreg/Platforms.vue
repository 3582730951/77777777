<template>
  <div class="p-6 space-y-4">
    <h1 class="text-2xl font-bold">Platforms</h1>

    <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
      <div v-for="p in platforms" :key="p.name" class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-4">
        <h2 class="text-lg font-semibold mb-2">{{ p.name }}</h2>
        <div class="text-sm space-y-1 text-gray-600 dark:text-gray-400">
          <div>Accounts: {{ p.account_count || 0 }}</div>
          <div>Status: <span :class="p.enabled ? 'text-green-600' : 'text-gray-400'">{{ p.enabled ? 'Enabled' : 'Disabled' }}</span></div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { autoregAPI } from '../../api'

const platforms = ref<any[]>([])

onMounted(async () => {
  try {
    const { data } = await autoregAPI.listPlatforms()
    platforms.value = data || []
  } catch {}
})
</script>
