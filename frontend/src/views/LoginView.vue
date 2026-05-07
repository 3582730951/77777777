<template>
  <div class="min-h-screen flex items-center justify-center bg-gradient-to-br from-gray-50 to-gray-100 dark:from-gray-950 dark:to-gray-900">
    <div class="w-full max-w-sm p-8 bg-white dark:bg-gray-900 rounded-2xl shadow-xl">
      <h1 class="text-2xl font-bold mb-6 text-center">LLM Pool</h1>
      <form @submit.prevent="handleLogin" class="space-y-4">
        <input v-model="username" type="text" placeholder="Username" autocomplete="username"
          class="w-full px-4 py-2.5 rounded-lg border border-gray-300 dark:border-gray-700 bg-transparent focus:ring-2 focus:ring-primary" />
        <input v-model="password" type="password" placeholder="Password" autocomplete="current-password"
          class="w-full px-4 py-2.5 rounded-lg border border-gray-300 dark:border-gray-700 bg-transparent focus:ring-2 focus:ring-primary" />
        <button type="submit" class="w-full py-2.5 bg-primary text-white rounded-lg font-medium hover:bg-primary-600 transition">
          Login
        </button>
        <p v-if="error" class="text-red-500 text-sm text-center">{{ error }}</p>
      </form>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import axios from 'axios'

const username = ref('')
const password = ref('')
const error = ref('')
const router = useRouter()
const auth = useAuthStore()

async function handleLogin() {
  try {
    const res = await axios.post('/api/admin/login', { username: username.value, password: password.value })
    auth.login(res.data.token || 'session')
    router.push('/')
  } catch (e: any) {
    error.value = e.response?.data?.error || 'Login failed'
  }
}
</script>
