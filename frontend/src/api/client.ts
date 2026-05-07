import axios from 'axios'

const client = axios.create({
  baseURL: '/api/admin',
  timeout: 30000,
})

client.interceptors.request.use(config => {
  const token = localStorage.getItem('pool_token')
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

client.interceptors.response.use(
  res => res.data,
  err => {
    if (err.response?.status === 401) {
      localStorage.removeItem('pool_token')
      window.location.href = '/login'
    }
    return Promise.reject(err)
  }
)

export default client
