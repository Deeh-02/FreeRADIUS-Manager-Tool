import axios from 'axios'
import { useAuthStore } from '@/store/auth'
import router from '@/router'

export const API_BASE = import.meta.env.VITE_API_URL || '/api/v1'

const api = axios.create({
  baseURL: API_BASE,
  timeout: 30000,
  headers: {
    'Content-Type': 'application/json',
  },
})

// Request interceptor: attach access token.
// Tokens live in localStorage (not an httpOnly cookie), so any script injected into the
// page could read them; the backend's Bearer-header design also makes CSRF a non-issue.
api.interceptors.request.use(
  (config) => {
    const token = localStorage.getItem('access_token')
    if (token) {
      config.headers.Authorization = `Bearer ${token}`
    }
    return config
  },
  (error) => Promise.reject(error)
)

let isRefreshing = false
let failedQueue = []

const processQueue = (error, token = null) => {
  failedQueue.forEach((prom) => {
    if (error) prom.reject(error)
    else prom.resolve(token)
  })
  failedQueue = []
}

// Response interceptor: handle 401 and token refresh.
// The backend distinguishes two 401s by `code`: SESSION_REVOKED (logged out / idle timeout
// -> drop credentials, go to /login) and TOKEN_EXPIRED (access JWT lapsed -> use the refresh
// token once). While one refresh is in flight, other failed requests wait in failedQueue
// and are replayed with the new token, so a burst of calls triggers a single refresh.
// The refresh token rotates: each refresh invalidates the previous one.
api.interceptors.response.use(
  (response) => response,
  async (error) => {
    const originalRequest = error.config

    // "Require MFA" is on and this account has no MFA yet: only the Profile page (MFA setup)
    // works until it is enabled.
    if (error.response?.status === 403 && error.response?.data?.code === 'MFA_SETUP_REQUIRED') {
      if (router.currentRoute.value.path !== '/profile') {
        router.push({ path: '/profile', query: { mfa: 'required' } })
      }
      return Promise.reject(error)
    }

    if (error.response?.status === 401 && !originalRequest._retry) {
      const code = error.response?.data?.code

      if (code === 'SESSION_REVOKED') {
        const authStore = useAuthStore()
        authStore.clearAuth()
        router.push('/login')
        return Promise.reject(error)
      }

      if (code === 'TOKEN_EXPIRED') {
        if (isRefreshing) {
          return new Promise((resolve, reject) => {
            failedQueue.push({ resolve, reject })
          })
            .then((token) => {
              originalRequest.headers.Authorization = `Bearer ${token}`
              return api(originalRequest)
            })
            .catch((err) => Promise.reject(err))
        }

        originalRequest._retry = true
        isRefreshing = true

        const refreshToken = localStorage.getItem('refresh_token')
        if (!refreshToken) {
          const authStore = useAuthStore()
          authStore.logout()
          router.push('/login')
          return Promise.reject(error)
        }

        try {
          // Plain axios (not `api`), so a failing refresh does not re-enter this interceptor.
          const { data } = await axios.post(`${API_BASE}/auth/refresh`, { refresh_token: refreshToken })
          localStorage.setItem('access_token', data.access_token)
          localStorage.setItem('refresh_token', data.refresh_token)
          api.defaults.headers.common.Authorization = `Bearer ${data.access_token}`
          processQueue(null, data.access_token)
          originalRequest.headers.Authorization = `Bearer ${data.access_token}`
          return api(originalRequest)
        } catch (refreshError) {
          processQueue(refreshError, null)
          const authStore = useAuthStore()
          authStore.logout()
          router.push('/login')
          return Promise.reject(refreshError)
        } finally {
          isRefreshing = false
        }
      }

      // Any other 401 on a signed-in request (e.g. the token was signed with an old
      // JWT_SECRET): the stored credentials are useless, so sign in again. A wrong password
      // on the login form is also a 401 and must stay on the form.
      const url = originalRequest.url || ''
      if (!url.startsWith('/auth/login') && localStorage.getItem('access_token')) {
        useAuthStore().clearAuth()
        router.push('/login')
      }
    }

    return Promise.reject(error)
  }
)

// --- Auth ---
export const authAPI = {
  login: (data) => api.post('/auth/login', data),
  logout: (refreshToken) => api.post('/auth/logout', { refresh_token: refreshToken }),
  refresh: (refreshToken) => api.post('/auth/refresh', { refresh_token: refreshToken }),
  getProfile: () => api.get('/auth/profile'),
  updateProfile: (data) => api.put('/auth/profile', data),
  changePassword: (data) => api.post('/auth/change-password', data),
  setupMFA: () => api.post('/auth/mfa/setup'),
  verifyMFA: (code) => api.post('/auth/mfa/verify', { code }),
}

// --- Dashboard ---
export const dashboardAPI = {
  getStats: () => api.get('/statistics/dashboard'),
  getActiveSessions: (params) => api.get('/sessions/active', { params }),
  getUserSessions: (username) => api.get(`/sessions/user/${username}`),
  getAuthLogs: (params) => api.get('/logs/auth', { params }),
  getAuditLogs: (params) => api.get('/logs/audit', { params }),
}

// --- Admin Users ---
export const adminAPI = {
  list: (params) => api.get('/admin/users', { params }),
  create: (data) => api.post('/admin/users', data),
  update: (id, data) => api.put(`/admin/users/${id}`, data),
  delete: (id) => api.delete(`/admin/users/${id}`),
}

// --- RADIUS Users ---
export const radiusUsersAPI = {
  list: (params) => api.get('/radius/users', { params }),
  get: (id) => api.get(`/radius/users/${id}`),
  create: (data) => api.post('/radius/users', data),
  update: (id, data) => api.put(`/radius/users/${id}`, data),
  delete: (id) => api.delete(`/radius/users/${id}`),
  resetPassword: (id, data) => api.post(`/radius/users/${id}/reset-password`, data),
  suspend: (id) => api.post(`/radius/users/${id}/suspend`),
  activate: (id) => api.post(`/radius/users/${id}/activate`),
  // Sends a RADIUS Disconnect-Request to the router; force also closes sessions the router
  // did not confirm (ones that are really gone but never got an accounting Stop).
  disconnect: (id, force = false) => api.post(`/radius/users/${id}/disconnect`, null, { params: force ? { force: 'true' } : {} }),
  getSessions: (id) => api.get(`/radius/users/${id}/sessions`),
  import: (formData) => api.post('/radius/users/import', formData, {
    headers: { 'Content-Type': 'multipart/form-data' },
  }),
  export: () => api.get('/radius/users/export', { responseType: 'blob' }),
}

// --- NAS Devices ---
export const nasAPI = {
  list: (params) => api.get('/nas', { params }),
  get: (id) => api.get(`/nas/${id}`),
  create: (data) => api.post('/nas', data),
  update: (id, data) => api.put(`/nas/${id}`, data),
  delete: (id) => api.delete(`/nas/${id}`),
  test: (id) => api.post(`/nas/${id}/test`),
  discover: (data) => api.post('/nas/discover', data),
}

// --- Network Scanner (nmap) ---
export const networkScannerAPI = {
  status: () => api.get('/network/scanner/status'),
  startScan: (data) => api.post('/network/scan', data),
  listScans: (params) => api.get('/network/scans', { params }),
  getScan: (id) => api.get(`/network/scans/${id}`),
  deleteScan: (id) => api.delete(`/network/scans/${id}`),
  importAsNAS: (hostId) => api.post(`/network/scans/hosts/${hostId}/import-nas`),
}

// --- RADIUS Test ---
export const radiusAPI = {
  test: (data) => api.post('/radius/test', data),
}

// --- Settings ---
export const settingsAPI = {
  get: () => api.get('/settings'),
  update: (data) => api.put('/settings', data),
  getBackups: () => api.get('/backups'),
  createBackup: () => api.post('/backup'),
  downloadBackup: (name) => api.get(`/backups/${encodeURIComponent(name)}`, { responseType: 'blob', timeout: 0 }),
  deleteBackup: (name) => api.delete(`/backups/${encodeURIComponent(name)}`),
  restoreInstructions: () => api.post('/restore'),
}

// saveBlob offers an axios blob response to the browser as a file download.
export function saveBlob(blob, filename) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// --- Health ---
export const healthAPI = {
  check: () => api.get('/health'),
}

// --- Setup Wizard ---
export const setupAPI = {
  status: () => api.get('/setup/status'),
  complete: (data) => api.post('/setup/complete', data),
}

// ── Tier 1 Pro: Vouchers ──────────────────────────────────────────────────
export const voucherAPI = {
  list: (params) => api.get('/vouchers', { params }),
  generate: (data) => api.post('/vouchers/generate', data),
  batches: () => api.get('/vouchers/batches'),
  disable: (id) => api.post(`/vouchers/${id}/disable`),
  delete: (id) => api.delete(`/vouchers/${id}`),
  export: (params) => api.get('/vouchers/export', { params, responseType: 'blob' }),
}

// ── Tier 1 Pro: Bandwidth Profiles ───────────────────────────────────────
export const bandwidthAPI = {
  list: () => api.get('/bandwidth-profiles'),
  create: (data) => api.post('/bandwidth-profiles', data),
  update: (id, data) => api.put(`/bandwidth-profiles/${id}`, data),
  delete: (id) => api.delete(`/bandwidth-profiles/${id}`),
  applyToUser: (userId, profileId) => api.post(`/radius/users/${userId}/bandwidth`, { profile_id: profileId }),
}

// ── Tier 1 Pro: Reports ───────────────────────────────────────────────────
export const reportsAPI = {
  usage: (params) => api.get('/reports/usage', { params }),
  daily: (params) => api.get('/reports/usage/daily', { params }),
  auth: (params) => api.get('/reports/auth', { params }),
  nas: (params) => api.get('/reports/nas', { params }),
  exportUsage: (params) => api.get('/reports/usage/export', { params, responseType: 'blob' }),
}

// ── Tier 2 Pro: User Plans ────────────────────────────────────────────────
// assignToUser applies the plan's limits and, for a paid plan, raises its invoice; the
// period starts when the invoice is paid unless start_now is set.
export const plansAPI = {
  list: () => api.get('/plans'),
  create: (data) => api.post('/plans', data),
  update: (id, data) => api.put(`/plans/${id}`, data),
  delete: (id) => api.delete(`/plans/${id}`),
  // data: { plan_id, start_now, promo_code }; plan_id null unlinks the plan.
  assignToUser: (userId, data) => api.post(`/radius/users/${userId}/plan`, data),
}

// ── Tier 2 Pro: Billing ───────────────────────────────────────────────────
export const billingAPI = {
  list: (params) => api.get('/invoices', { params }),
  get: (id) => api.get(`/invoices/${id}`),
  create: (data) => api.post('/invoices', data),
  update: (id, data) => api.put(`/invoices/${id}`, data),
  delete: (id) => api.delete(`/invoices/${id}`),
}

// ── Tier 2 Pro: NAS Status ────────────────────────────────────────────────
// nasAPI extended with status + ping
export const nasStatusAPI = {
  status: () => api.get('/nas/status'),
  pingNow: (id) => api.post(`/nas/${id}/ping`),
}

// ── Tier 2 Pro: Alert Rules ───────────────────────────────────────────────
export const alertsAPI = {
  list: () => api.get('/alerts'),
  create: (data) => api.post('/alerts', data),
  update: (id, data) => api.put(`/alerts/${id}`, data),
  delete: (id) => api.delete(`/alerts/${id}`),
  testEmail: (data) => api.post('/alerts/test-email', data),
}

// ── Tier 3 Pro: IP Pools ──────────────────────────────────────────────────
export const ipPoolsAPI = {
  list: () => api.get('/ip-pools'),
  create: (data) => api.post('/ip-pools', data),
  delete: (id) => api.delete(`/ip-pools/${id}`),
  listIPs: (poolId) => api.get(`/ip-pools/${poolId}/ips`),
  assign: (data) => api.post('/ip-pools/assign', data),
  release: (data) => api.post('/ip-pools/release', data),
}

// ── Tier 3 Pro: API Keys ──────────────────────────────────────────────────
export const apiKeysAPI = {
  list: () => api.get('/api-keys'),
  create: (data) => api.post('/api-keys', data),
  revoke: (id) => api.post(`/api-keys/${id}/revoke`),
  delete: (id) => api.delete(`/api-keys/${id}`),
  stats: () => api.get('/api-keys/stats'),
}

// ── Tier 3 Pro: Scheduler ─────────────────────────────────────────────────
export const schedulerAPI = {
  list: () => api.get('/scheduler'),
  toggle: (id) => api.post(`/scheduler/${id}/toggle`),
  runNow: (id) => api.post(`/scheduler/${id}/run`),
  updateSchedule: (id, schedule) => api.put(`/scheduler/${id}/schedule`, { schedule }),
}

// ── Tier 3 Pro: Bulk Import / Export ─────────────────────────────────────
export const importAPI = {
  importCSV: (formData) => api.post('/radius/users/import', formData, {
    headers: { 'Content-Type': 'multipart/form-data' },
  }),
  exportCSV: () => api.get('/radius/users/export', { responseType: 'blob' }),
}

// ── Tier 4 Pro: Hotspot Zones ─────────────────────────────────────────────
export const zonesAPI = {
  list: () => api.get('/zones'),
  create: (data) => api.post('/zones', data),
  update: (id, data) => api.put(`/zones/${id}`, data),
  delete: (id) => api.delete(`/zones/${id}`),
  stats: (id) => api.get(`/zones/${id}/stats`),
  assignNAS: (data) => api.post('/zones/assign-nas', data),
}

// ── Tier 4 Pro: SMS ───────────────────────────────────────────────────────
export const smsAPI = {
  send: (data) => api.post('/sms/send', data),
  logs: (params) => api.get('/sms/logs', { params }),
  notifyExpiry: (data) => api.post('/sms/notify-expiry', data),
  config: () => api.get('/sms/config'),
}

// ── Tier 4 Pro: Live Stats (server-sent events) ───────────────────────────
// EventSource cannot send an Authorization header and the API no longer accepts tokens in
// the URL, so the stream is read with fetch. openStream returns an object with close(), like
// EventSource; onMessage gets each parsed `data:` payload. It reconnects after errors (with
// the current token, so a refreshed token is picked up) until closed.
export const liveStatsAPI = {
  current: () => api.get('/live/stats/current'),
  openStream: (onMessage, onState = () => {}) => {
    let closed = false
    let controller = null
    const run = async () => {
      while (!closed) {
        controller = new AbortController()
        try {
          const res = await fetch(`${API_BASE}/live/stats`, {
            headers: { Authorization: `Bearer ${localStorage.getItem('access_token') || ''}` },
            signal: controller.signal,
          })
          if (res.status === 401) {
            // Let the axios interceptor refresh the token, then reconnect.
            await api.get('/live/stats/current').catch(() => {})
            throw new Error('unauthorized')
          }
          if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`)
          onState('open')
          const reader = res.body.getReader()
          const decoder = new TextDecoder()
          let buf = ''
          for (;;) {
            const { value, done } = await reader.read()
            if (done) break
            buf += decoder.decode(value, { stream: true })
            let idx
            while ((idx = buf.indexOf('\n\n')) >= 0) {
              const event = buf.slice(0, idx)
              buf = buf.slice(idx + 2)
              const data = event.split('\n').filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trim()).join('\n')
              if (data) {
                try { onMessage(JSON.parse(data)) } catch { /* ignore malformed event */ }
              }
            }
          }
        } catch {
          if (closed) return
        }
        if (closed) return
        onState('error')
        await new Promise((r) => setTimeout(r, 5000))
      }
    }
    run()
    return { close: () => { closed = true; controller?.abort() } }
  },
}

// ── Tier 5 Pro: Organizations ─────────────────────────────────────────────
export const orgsAPI = {
  list: () => api.get('/organizations'),
  create: (data) => api.post('/organizations', data),
  update: (id, data) => api.put(`/organizations/${id}`, data),
  delete: (id) => api.delete(`/organizations/${id}`),
  stats: (id) => api.get(`/organizations/${id}/stats`),
  assignUser: (data) => api.post('/organizations/assign-user', data),
}

// ── Tier 5 Pro: Customers CRM ─────────────────────────────────────────────
export const customersAPI = {
  list: (params) => api.get('/customers', { params }),
  get: (id) => api.get(`/customers/${id}`),
  create: (data) => api.post('/customers', data),
  update: (id, data) => api.put(`/customers/${id}`, data),
  delete: (id) => api.delete(`/customers/${id}`),
}

// ── Tier 5 Pro: Support Tickets ───────────────────────────────────────────
export const ticketsAPI = {
  list: (params) => api.get('/tickets', { params }),
  create: (data) => api.post('/tickets', data),
  update: (id, data) => api.put(`/tickets/${id}`, data),
  delete: (id) => api.delete(`/tickets/${id}`),
}

// ── Tier 5 Pro: Captive Portals ───────────────────────────────────────────
export const captiveAPI = {
  list: () => api.get('/captive'),
  create: (data) => api.post('/captive', data),
  update: (id, data) => api.put(`/captive/${id}`, data),
  delete: (id) => api.delete(`/captive/${id}`),
}

// ── Tier 5 Pro: Webhooks ──────────────────────────────────────────────────
export const webhooksAPI = {
  list: () => api.get('/webhooks'),
  create: (data) => api.post('/webhooks', data),
  update: (id, data) => api.put(`/webhooks/${id}`, data),
  delete: (id) => api.delete(`/webhooks/${id}`),
  test: (id) => api.post(`/webhooks/${id}/test`),
  logs: (id) => api.get(`/webhooks/${id}/logs`),
}

// ── Tier 6 Pro: Payments ──────────────────────────────────────────────────
// Payments are never deleted, only reversed (the invoice re-opens and the plan period it
// granted is taken back).
export const paymentsAPI = {
  list: (params) => api.get('/payments', { params }),
  create: (data, config) => api.post('/payments', data, config),
  reverse: (id, reason) => api.post(`/payments/${id}/reverse`, { reason }),
  summary: () => api.get('/payments/summary'),
  receipt: (id) => api.get(`/payments/${id}/receipt`, { responseType: 'text' }),
}

export const mpesaAPI = {
  status: () => api.get('/mpesa-admin/status'),
  prompt: (invoiceId, phone) => api.post('/mpesa-admin/prompt', { invoice_id: invoiceId, phone }),
  promptStatus: (id) => api.get(`/mpesa-admin/prompt/${id}`),
  prompts: (params) => api.get('/mpesa-admin/prompts', { params }),
  c2b: (params) => api.get('/mpesa-admin/c2b', { params }),
  allocate: (id, username, invoiceId) => api.post(`/mpesa-admin/c2b/${id}/allocate`, { username, invoice_id: invoiceId || null }),
  registerC2B: () => api.post('/mpesa-admin/register-c2b'),
}

// openReceipt fetches a receipt with the auth header and shows it in a new tab (the
// receipt page is self-contained HTML with a Print button).
export async function openReceipt(id) {
  // Opened before the request so popup blockers treat it as a direct click.
  const win = window.open('', '_blank')
  let data
  try {
    ({ data } = await paymentsAPI.receipt(id))
  } catch (e) {
    win?.close()
    throw e
  }
  const url = URL.createObjectURL(new Blob([data], { type: 'text/html' }))
  if (win) win.location.href = url
  else window.location.href = url
  setTimeout(() => URL.revokeObjectURL(url), 60000)
}

// ── Tier 6 Pro: RADIUS Attribute Templates ────────────────────────────────
export const templatesAPI = {
  list: () => api.get('/templates'),
  create: (data) => api.post('/templates', data),
  update: (id, data) => api.put(`/templates/${id}`, data),
  delete: (id) => api.delete(`/templates/${id}`),
  apply: (id, data) => api.post(`/templates/${id}/apply`, data),
  clone: (id, data) => api.post(`/templates/${id}/clone`, data),
}

// ── Tier 6 Pro: Promotions ────────────────────────────────────────────────
// A code reduces what is billed when it is given with an invoice or a plan assignment
// (promo_code); validate only previews the discount.
export const promotionsAPI = {
  list: () => api.get('/promotions'),
  create: (data) => api.post('/promotions', data),
  update: (id, data) => api.put(`/promotions/${id}`, data),
  delete: (id) => api.delete(`/promotions/${id}`),
  validate: (data) => api.post('/promotions/validate', data),
  apply: (data) => api.post('/promotions/apply', data),
}

// ── Tier 6 Pro: Bulk Operations ───────────────────────────────────────────
export const bulkAPI = {
  execute: (data) => api.post('/bulk', data),
  history: () => api.get('/bulk/history'),
}

// ── Tier 7: Security Suite ────────────────────────────────────────────────
export const securityAPI = {
  getSummary: () => api.get('/security/summary'),
  listAlerts: (params) => api.get('/security/alerts', { params }),
  ackAlert: (id) => api.put(`/security/alerts/${id}/ack`),
  ackAllAlerts: () => api.put('/security/alerts/ack-all'),
  deleteAlert: (id) => api.delete(`/security/alerts/${id}`),
  getBlockedIPs: () => api.get('/security/blocked-ips'),
  blockIP: (data) => api.post('/security/blocked-ips', data),
  unblockIP: (id) => api.delete(`/security/blocked-ips/${id}`),
  geoipLookup: (ip) => api.get('/security/geoip/lookup', { params: { ip } }),
  listGeoIPRules: () => api.get('/security/geoip/rules'),
  createGeoIPRule: (data) => api.post('/security/geoip/rules', data),
  deleteGeoIPRule: (id) => api.delete(`/security/geoip/rules/${id}`),
  honeypotStatus: () => api.get('/security/honeypot/status'),
  listHoneypotLogs: (params) => api.get('/security/honeypot/logs', { params }),
  clearHoneypotLogs: (data) => api.delete('/security/honeypot/logs', { data }),
  simulateAuth: (data) => api.post('/radius/simulate', data),
  simulateBatch: (data) => api.post('/radius/simulate/batch', data),
}

export default api
