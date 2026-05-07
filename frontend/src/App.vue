<template>
  <div class="flex h-screen">
    <aside v-if="isAuthenticated" class="w-56 bg-white dark:bg-gray-900 border-r border-gray-200 dark:border-gray-800 flex flex-col">
      <div class="p-4 font-bold text-lg">LLM Pool</div>
      <nav class="flex-1 px-2 space-y-1">
        <router-link v-for="item in navItems" :key="item.path" :to="item.path"
          class="block px-3 py-2 rounded-lg text-sm hover:bg-gray-100 dark:hover:bg-gray-800"
          active-class="bg-primary-50 dark:bg-primary-700/20 text-primary font-medium">
          {{ item.label }}
        </router-link>
      </nav>
    </aside>
    <main class="flex-1 overflow-auto">
      <router-view />
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useAuthStore } from './stores/auth'

const auth = useAuthStore()
const isAuthenticated = computed(() => auth.isLoggedIn)

const navItems = [
  { path: '/', label: 'Dashboard' },
  { path: '/accounts', label: 'Accounts' },
  { path: '/groups', label: 'Groups' },
  { path: '/keys', label: 'API Keys' },
  { path: '/tenants', label: 'Tenants' },
  { path: '/audit', label: 'Audit Log' },
  { path: '/autoreg/tasks', label: 'AutoReg Tasks' },
  { path: '/autoreg/platforms', label: 'Platforms' },
  { path: '/autoreg/stats', label: 'Reg Stats' },
]
</script>
