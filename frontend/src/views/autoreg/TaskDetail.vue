<template>
  <div class="p-6 space-y-4">
    <div class="flex items-center gap-4">
      <router-link to="/autoreg/tasks" class="text-primary hover:underline text-sm">&larr; Back</router-link>
      <h1 class="text-2xl font-bold">Task #{{ taskId }}</h1>
    </div>

    <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 p-4">
      <div class="font-mono text-xs whitespace-pre-wrap max-h-[70vh] overflow-y-auto" ref="logEl">
        <div v-for="(line, i) in logs" :key="i" class="py-0.5" :class="line.startsWith('[ERROR') ? 'text-red-500' : ''">{{ line }}</div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute } from 'vue-router'

const route = useRoute()
const taskId = route.params.id as string
const logs = ref<string[]>([])
const logEl = ref<HTMLElement>()
let evtSource: EventSource | null = null

onMounted(() => {
  const baseUrl = (window as any).__ADMIN_BASE || ''
  evtSource = new EventSource(`${baseUrl}/api/autoreg/tasks/${taskId}/logs/stream`)
  evtSource.onmessage = (ev) => {
    logs.value.push(ev.data)
    nextTick(() => {
      if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
    })
  }
  evtSource.onerror = () => {
    logs.value.push('[stream ended]')
  }
})

onUnmounted(() => {
  evtSource?.close()
})
</script>
