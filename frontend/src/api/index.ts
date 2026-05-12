import client from './client'

export const accountsAPI = {
  list: () => client.get('/accounts'),
  bulkImport: (accounts: any[]) => client.post('/accounts/bulk-import', accounts),
  probe: (id: string) => client.post(`/accounts/${id}/probe`),
  discover: (id: string) => client.post(`/accounts/${id}/discover`),
  delete: (id: string) => client.delete(`/accounts/${id}`),
}

export const groupsAPI = {
  list: (tenantId?: string) => client.get('/groups', { params: { tenant_id: tenantId } }),
  upsert: (data: any) => client.post('/groups', data),
  delete: (id: string) => client.delete(`/groups/${id}`),
}

export const keysAPI = {
  list: (tenantId?: string, groupId?: string) => client.get('/api-keys', { params: { tenant_id: tenantId, group_id: groupId } }),
  create: (data: any) => client.post('/api-keys', data),
  revoke: (value: string) => client.delete(`/api-keys/${value}`),
}

export const tenantsAPI = {
  list: () => client.get('/tenants'),
  create: (data: any) => client.post('/tenants', data),
  delete: (id: string) => client.delete(`/tenants/${id}`),
}

export const chartsAPI = {
  requests: (window?: string) => client.get('/charts/requests', { params: { window } }),
  accounts: () => client.get('/charts/accounts'),
  cacheHit: (window?: string) => client.get('/charts/cache-hit', { params: { window } }),
  cacheHitSeries: (window?: string) => client.get('/charts/cache-hit-series', { params: { window } }),
  cacheHitByKey: () => client.get('/charts/cache-hit-by-key'),
}

export const auditAPI = {
  list: (limit?: number) => client.get('/audit', { params: { limit } }),
}

// Autoreg API - proxied through gateway /api/autoreg -> Python service
import axios from 'axios'
const autoregClient = axios.create({ baseURL: '/api/autoreg', timeout: 30000 })
autoregClient.interceptors.request.use(config => {
  const token = localStorage.getItem('pool_token')
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

export const autoregAPI = {
  listTasks: () => autoregClient.get('/tasks'),
  createTask: (data: any) => autoregClient.post('/tasks/register', data),
  getTask: (id: string) => autoregClient.get(`/tasks/${id}`),
  listPlatforms: () => autoregClient.get('/platforms'),
  getStats: () => autoregClient.get('/accounts/stats'),
  listProviders: () => autoregClient.get('/provider-settings'),
}
