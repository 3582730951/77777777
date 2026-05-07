import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', component: () => import('../views/LoginView.vue'), meta: { public: true } },
    { path: '/', component: () => import('../views/DashboardView.vue') },
    { path: '/accounts', component: () => import('../views/AccountsView.vue') },
    { path: '/groups', component: () => import('../views/GroupsView.vue') },
    { path: '/keys', component: () => import('../views/KeysView.vue') },
    { path: '/tenants', component: () => import('../views/TenantsView.vue') },
    { path: '/audit', component: () => import('../views/AuditView.vue') },
    { path: '/autoreg/tasks', component: () => import('../views/autoreg/TaskList.vue') },
    { path: '/autoreg/tasks/create', component: () => import('../views/autoreg/TaskCreate.vue') },
    { path: '/autoreg/tasks/:id', component: () => import('../views/autoreg/TaskDetail.vue') },
    { path: '/autoreg/platforms', component: () => import('../views/autoreg/Platforms.vue') },
    { path: '/autoreg/stats', component: () => import('../views/autoreg/Stats.vue') },
  ],
})

router.beforeEach((to) => {
  const token = localStorage.getItem('pool_token')
  if (!to.meta.public && !token) return '/login'
})

export default router
