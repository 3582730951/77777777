import { defineStore } from 'pinia'
import { ref, computed } from 'vue'

export const useAuthStore = defineStore('auth', () => {
  const token = ref(localStorage.getItem('pool_token') || '')
  const isLoggedIn = computed(() => !!token.value)

  function login(t: string) {
    token.value = t
    localStorage.setItem('pool_token', t)
  }

  function logout() {
    token.value = ''
    localStorage.removeItem('pool_token')
  }

  return { token, isLoggedIn, login, logout }
})
