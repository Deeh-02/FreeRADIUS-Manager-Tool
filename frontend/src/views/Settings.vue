<template>
  <div class="space-y-6">
    <div class="page-header">
      <div>
        <h1 class="page-title">Settings</h1>
        <p class="text-sm text-gray-500 mt-0.5">Business rules, security and backups. Changes apply immediately.</p>
      </div>
      <button @click="saveSettings" class="btn-primary" :disabled="saving || !dirtyKeys.length">
        <span v-if="saving" class="w-4 h-4 border-2 border-white/30 border-t-white rounded-full spinner"></span>
        <CheckIcon v-else class="w-4 h-4" />
        Save Changes<span v-if="dirtyKeys.length"> ({{ dirtyKeys.length }})</span>
      </button>
    </div>

    <div v-if="loading" class="h-64 flex items-center justify-center">
      <span class="w-8 h-8 border-2 border-blue-600 border-t-transparent rounded-full spinner"></span>
    </div>

    <div v-else class="grid grid-cols-1 lg:grid-cols-2 gap-6">
      <div v-for="section in sections" :key="section.title" class="card space-y-4" :class="section.wide ? 'lg:col-span-2' : ''">
        <h3 class="font-semibold text-gray-900 flex items-center gap-2">
          <component :is="section.icon" class="w-5 h-5 text-blue-600" />
          {{ section.title }}
        </h3>
        <p v-if="section.note" class="text-xs text-gray-500 -mt-2">{{ section.note }}</p>
        <div :class="section.wide ? 'grid grid-cols-1 md:grid-cols-2 gap-4' : 'space-y-4'">
          <template v-for="f in section.fields" :key="f.key">
            <div v-if="values[f.key] !== undefined" :class="f.type === 'textarea' ? 'md:col-span-2' : ''">
              <label v-if="f.type === 'bool'" class="flex items-start gap-2 text-sm text-gray-700">
                <input type="checkbox" class="rounded mt-0.5" :checked="values[f.key] === 'true'"
                       @change="values[f.key] = $event.target.checked ? 'true' : 'false'" />
                <span>{{ f.label }}<span v-if="f.help" class="block text-xs text-gray-500">{{ f.help }}</span></span>
              </label>
              <template v-else>
                <label class="form-label">{{ f.label }}</label>
                <textarea v-if="f.type === 'textarea'" v-model="values[f.key]" rows="2" class="form-input"></textarea>
                <input v-else v-model="values[f.key]" :type="f.type || 'text'" :min="f.min" :max="f.max"
                       :maxlength="f.maxlength" class="form-input" :class="f.upper ? 'uppercase' : ''" />
                <p v-if="f.help" class="text-xs text-gray-500 mt-1">{{ f.help }}</p>
              </template>
            </div>
          </template>
        </div>
      </div>

      <!-- Email -->
      <div class="card space-y-4">
        <h3 class="font-semibold text-gray-900 flex items-center gap-2">
          <EnvelopeIcon class="w-5 h-5 text-blue-600" />
          Email (SMTP)
        </h3>
        <p class="text-sm text-gray-600">
          The mail server is set in <code class="bg-gray-100 px-1 rounded">.env</code>
          (SMTP_HOST, SMTP_PORT 587, SMTP_USER, SMTP_PASS, SMTP_FROM). It sends alerts, the daily report and expiry reminders.
        </p>
        <div class="flex gap-2">
          <input v-model.trim="testEmailTo" type="email" class="form-input" placeholder="you@example.com" />
          <button @click="testEmail" class="btn-secondary whitespace-nowrap" :disabled="!testEmailTo || sendingEmail">
            <PaperAirplaneIcon class="w-4 h-4" />
            Send test
          </button>
        </div>
      </div>

      <!-- Backups -->
      <div class="card space-y-4">
        <h3 class="font-semibold text-gray-900 flex items-center gap-2">
          <ArchiveBoxIcon class="w-5 h-5 text-blue-600" />
          Backups
        </h3>
        <p class="text-xs text-gray-500 -mt-2">
          The "Database Backup" scheduled task writes a dump every day to the server's backups folder and deletes ones older
          than the retention above. Download copies regularly: a backup on the same disk does not survive losing that disk.
        </p>
        <div class="flex gap-2">
          <button @click="createBackup" class="btn-secondary" :disabled="backups.running">
            <span v-if="backups.running" class="w-4 h-4 border-2 border-gray-300 border-t-gray-600 rounded-full spinner"></span>
            <ArchiveBoxIcon v-else class="w-4 h-4" />
            {{ backups.running ? 'Backup running…' : 'Back up now' }}
          </button>
          <button @click="loadBackups" class="btn-secondary"><ArrowPathIcon class="w-4 h-4" /></button>
          <button @click="showRestore" class="btn-secondary">How to restore</button>
        </div>
        <p v-if="backups.last_error" class="text-xs text-red-600">Last backup failed: {{ backups.last_error }}</p>
        <div class="max-h-56 overflow-y-auto divide-y divide-gray-100 border border-gray-100 rounded-lg">
          <p v-if="!backups.backups?.length" class="text-sm text-gray-400 p-3">No backups yet.</p>
          <div v-for="b in backups.backups" :key="b.name" class="flex items-center justify-between px-3 py-2 text-sm">
            <div>
              <p class="font-mono text-xs">{{ b.name }}</p>
              <p class="text-xs text-gray-400">{{ new Date(b.created_at).toLocaleString() }} · {{ size(b.size_bytes) }}</p>
            </div>
            <div class="flex gap-1">
              <button @click="download(b)" class="p-1.5 rounded hover:bg-blue-50 text-gray-400 hover:text-blue-600" title="Download">
                <ArrowDownTrayIcon class="w-4 h-4" />
              </button>
              <button @click="removeBackup(b)" class="p-1.5 rounded hover:bg-red-50 text-gray-400 hover:text-red-600" title="Delete">
                <TrashIcon class="w-4 h-4" />
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- RADIUS Test -->
      <div class="card space-y-4 lg:col-span-2">
        <h3 class="font-semibold text-gray-900 flex items-center gap-2">
          <SignalIcon class="w-5 h-5 text-blue-600" />
          RADIUS Test
        </h3>
        <p class="text-sm text-gray-600">Sends a real login request to FreeRADIUS, as a router would.</p>
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
          <div>
            <label class="form-label">Username</label>
            <input v-model="radiusTest.username" type="text" class="form-input" />
          </div>
          <div>
            <label class="form-label">Password</label>
            <input v-model="radiusTest.password" type="password" class="form-input" />
          </div>
          <div>
            <label class="form-label">NAS (optional)</label>
            <input v-model="radiusTest.nasname" type="text" class="form-input" placeholder="Leave blank for default" />
          </div>
        </div>
        <button @click="runRadiusTest" class="btn-primary" :disabled="testingRadius">
          <span v-if="testingRadius" class="w-4 h-4 border-2 border-white/30 border-t-white rounded-full spinner"></span>
          Run Test
        </button>
        <div v-if="radiusTestResult" class="p-4 rounded-lg border"
          :class="radiusTestResult.success ? 'bg-green-50 border-green-200 text-green-800' : 'bg-red-50 border-red-200 text-red-800'"
        >
          <p class="font-semibold">{{ radiusTestResult.success ? '✓ Authentication Successful' : '✗ Authentication Failed' }}</p>
          <p class="text-sm mt-1">{{ radiusTestResult.message }}</p>
          <p class="text-xs mt-1 opacity-75">Latency: {{ radiusTestResult.latency_ms?.toFixed(2) }}ms | Server: {{ radiusTestResult.nasname }}</p>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useToast } from 'vue-toastification'
import { settingsAPI, radiusAPI, alertsAPI, saveBlob } from '@/api'
import { apiError } from '@/utils/money'
import {
  CheckIcon, LockClosedIcon, ShieldCheckIcon, EnvelopeIcon, BanknotesIcon, UsersIcon, BellAlertIcon,
  ArchiveBoxIcon, SignalIcon, ArrowDownTrayIcon, PaperAirplaneIcon, TrashIcon, ArrowPathIcon,
} from '@heroicons/vue/24/outline'

const toast = useToast()
const loading = ref(false)
const saving = ref(false)
const testingRadius = ref(false)
const radiusTestResult = ref(null)
const radiusTest = ref({ username: '', password: '', nasname: '' })
const testEmailTo = ref('')
const sendingEmail = ref(false)
const backups = ref({ backups: [] })
let backupPoll = null

// Only keys the backend reads. A field is shown when the key exists in system_settings.
const sections = [
  {
    title: 'Business & Billing', icon: BanknotesIcon, wide: true,
    note: 'Prepaid: a customer on a paid plan is online while their paid-until date has not passed. Paying an invoice adds the plan\'s days.',
    fields: [
      { key: 'org_name', label: 'Business name', help: 'Shown on receipts and the customer portal.' },
      { key: 'org_email', label: 'Business email', type: 'email', help: 'Shown on receipts.' },
      { key: 'default_currency', label: 'Default currency', maxlength: 3, upper: true, help: 'ISO code, e.g. KES. Used when a plan or invoice does not name one.' },
      { key: 'invoice_due_days', label: 'Invoice due after (days)', type: 'number', min: 0, max: 365 },
      { key: 'renewal_invoice_days', label: 'Raise renewal invoice (days before paid-until)', type: 'number', min: 0, max: 60, help: 'The "Raise Renewal Invoices" task bills customers this many days before they run out.' },
      { key: 'billing_grace_days', label: 'Grace days after paid-until', type: 'number', min: 0, max: 60, help: 'Customers stay online this many days after their paid-until date.' },
      { key: 'suspend_overdue', label: 'Block customers with overdue postpaid invoices', type: 'bool', help: 'For invoices whose period was started before payment ("start now").' },
      { key: 'report_email', label: 'Daily report email', type: 'email', help: 'Empty = not emailed.' },
      { key: 'reminder_days', label: 'Expiry reminder (days before)', type: 'number', min: 0, max: 60, help: '0 = no reminders. Sent by SMS (phone) or email.' },
      { key: 'reminder_message', label: 'Reminder text', type: 'textarea', help: 'Placeholders: {username} {expiry} {amount}' },
    ],
  },
  {
    title: 'Staff Security', icon: ShieldCheckIcon,
    fields: [
      { key: 'password_min_length', label: 'Minimum staff password length', type: 'number', min: 8, max: 128 },
      { key: 'session_timeout', label: 'Idle sign-out (seconds)', type: 'number', min: 300, max: 604800 },
      { key: 'brute_force_attempts', label: 'Failed logins before lockout', type: 'number', min: 1, max: 100 },
      { key: 'brute_force_lockout', label: 'Lockout duration (minutes)', type: 'number', min: 1, max: 1440 },
      { key: 'mfa_required', label: 'Require two-factor authentication for all staff', type: 'bool', help: 'Staff without it can only use their Profile page until they set it up. Set up your own first.' },
    ],
  },
  {
    title: 'Customer Accounts', icon: UsersIcon,
    fields: [
      { key: 'max_device_limit', label: 'Highest device limit allowed per customer', type: 'number', min: 1, max: 500 },
      { key: 'password_expiry_days', label: 'Customer password expiry stamp (days)', type: 'number', min: 1, max: 3650, help: 'Recorded on new accounts; not enforced at login.' },
      { key: 'portal_max_attempts', label: 'Customer portal: failed logins before lockout', type: 'number', min: 1, max: 100 },
      { key: 'portal_lockout_minutes', label: 'Customer portal: lockout (minutes)', type: 'number', min: 1, max: 1440 },
    ],
  },
  {
    title: 'Attack Detection', icon: BellAlertIcon,
    note: 'Credential stuffing: many failed logins from one IP in a short window blocks that IP from the staff and customer logins.',
    fields: [
      { key: 'cs_max_fails', label: 'Failed logins that trigger a block', type: 'number', min: 1, max: 10000 },
      { key: 'cs_window_secs', label: 'Within (seconds)', type: 'number', min: 10, max: 86400 },
      { key: 'cs_block_mins', label: 'Block for (minutes)', type: 'number', min: 1, max: 100000 },
    ],
  },
  {
    title: 'Backup Retention', icon: LockClosedIcon,
    fields: [
      { key: 'backup_retention_days', label: 'Keep daily backups for (days)', type: 'number', min: 0, max: 3650, help: '0 = keep forever.' },
    ],
  },
]

const values = ref({})
const original = ref({})
const dirtyKeys = computed(() => Object.keys(values.value).filter((k) => values.value[k] !== original.value[k]))

async function loadSettings() {
  loading.value = true
  try {
    const { data } = await settingsAPI.get()
    const known = new Set(sections.flatMap((s) => s.fields.map((f) => f.key)))
    const v = {}
    Object.entries(data).forEach(([k, entry]) => { if (known.has(k)) v[k] = entry.value })
    values.value = { ...v }
    original.value = { ...v }
  } catch (err) { toast.error(apiError(err, 'Failed to load settings')) }
  finally { loading.value = false }
}

async function saveSettings() {
  saving.value = true
  try {
    const updates = {}
    dirtyKeys.value.forEach((k) => { updates[k] = String(values.value[k]).trim() })
    if (updates.default_currency) updates.default_currency = updates.default_currency.toUpperCase()
    await settingsAPI.update(updates)
    original.value = { ...values.value, ...updates }
    values.value = { ...original.value }
    toast.success('Settings saved')
  } catch (err) { toast.error(apiError(err, 'Failed to save settings')) }
  finally { saving.value = false }
}

async function loadBackups() {
  try {
    backups.value = (await settingsAPI.getBackups()).data
  } catch (err) { toast.error(apiError(err, 'Failed to list backups')) }
  clearTimeout(backupPoll)
  if (backups.value.running) backupPoll = setTimeout(loadBackups, 3000)
}

async function createBackup() {
  try {
    const { data } = await settingsAPI.createBackup()
    toast.info(data.message)
    backups.value.running = true
    setTimeout(loadBackups, 1500)
  } catch (err) { toast.error(apiError(err, 'Backup failed')) }
}

async function download(b) {
  try {
    const { data } = await settingsAPI.downloadBackup(b.name)
    saveBlob(data, b.name)
  } catch (err) { toast.error(apiError(err, 'Download failed')) }
}

async function removeBackup(b) {
  if (!confirm(`Delete ${b.name}? Make sure you have a copy elsewhere.`)) return
  try {
    await settingsAPI.deleteBackup(b.name)
    loadBackups()
  } catch (err) { toast.error(apiError(err, 'Delete failed')) }
}

async function showRestore() {
  try {
    const { data } = await settingsAPI.restoreInstructions()
    alert(data.message)
  } catch (err) { toast.error(apiError(err)) }
}

function size(bytes) {
  if (bytes > 1048576) return `${(bytes / 1048576).toFixed(1)} MB`
  return `${Math.max(1, Math.round(bytes / 1024))} KB`
}

async function testEmail() {
  sendingEmail.value = true
  try {
    const { data } = await alertsAPI.testEmail({ to: testEmailTo.value })
    toast.success(data.message)
  } catch (err) { toast.error(apiError(err, 'Test email failed')) }
  finally { sendingEmail.value = false }
}

async function runRadiusTest() {
  testingRadius.value = true
  radiusTestResult.value = null
  try {
    const { data } = await radiusAPI.test(radiusTest.value)
    radiusTestResult.value = data
  } catch (err) {
    radiusTestResult.value = { success: false, message: apiError(err, 'Test failed'), latency_ms: 0 }
  } finally {
    testingRadius.value = false
  }
}

onMounted(() => { loadSettings(); loadBackups() })
onUnmounted(() => clearTimeout(backupPoll))
</script>
