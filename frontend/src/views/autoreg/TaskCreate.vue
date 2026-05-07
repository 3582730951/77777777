<template>
  <div class="p-6 space-y-4 max-w-2xl">
    <h1 class="text-2xl font-bold">Create Registration Task</h1>

    <form @submit.prevent="submit" class="space-y-4 bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-6">
      <div>
        <label class="block text-sm font-medium mb-1">Platform</label>
        <select v-model="form.platform" class="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-700 bg-transparent">
          <option v-for="p in platforms" :key="p" :value="p">{{ p }}</option>
        </select>
      </div>

      <div>
        <label class="block text-sm font-medium mb-1">Count</label>
        <input v-model.number="form.count" type="number" min="1" max="100" class="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-700 bg-transparent" />
      </div>

      <div>
        <label class="block text-sm font-medium mb-1">Concurrency</label>
        <input v-model.number="form.concurrency" type="number" min="1" max="10" class="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-700 bg-transparent" />
      </div>

      <div class="flex gap-2">
        <button type="submit" :disabled="loading" class="px-4 py-2 bg-primary text-white rounded-lg text-sm">
          {{ loading ? 'Creating...' : 'Create Task' }}
        </button>
        <router-link to="/autoreg/tasks" class="px-4 py-2 border border-gray-300 dark:border-gray-700 rounded-lg text-sm">Cancel</router-link>
      </div>
    </form>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { autoregAPI } from '../../api'

const router = useRouter()
const loading = ref(false)
const platforms = ref<string[]>(['chatgpt', 'cursor', 'kiro', 'windsurf', 'grok', 'trae', 'blink', 'cerebras', 'tavily', 'openblocklabs'])

const form = ref({
  platform: 'chatgpt',
  count: 1,
  concurrency: 1,
})

async function submit() {
  loading.value = true
  try {
    await autoregAPI.createTask(form.value)
    router.push('/autoreg/tasks')
  } catch (e) {
    alert('Failed to create task')
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  try {
    const { data } = await autoregAPI.listPlatforms()
    if (data && Array.isArray(data)) {
      platforms.value = data.map((p: any) => p.name || p)
    }
  } catch {}
})
</script>
