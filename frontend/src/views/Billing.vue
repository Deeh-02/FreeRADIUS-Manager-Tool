<template>
  <div class="space-y-5">
    <div class="page-header">
      <div>
        <h1 class="page-title">Billing</h1>
        <p class="text-sm text-gray-500 mt-0.5">Invoices. An invoice is paid by recording payments against it.</p>
      </div>
      <button @click="openCreate" class="btn-primary" v-if="authStore.isAdmin">
        <PlusIcon class="w-4 h-4" />
        New Invoice
      </button>
    </div>

    <!-- Summary row (all invoices, never mixing currencies) -->
    <div class="grid grid-cols-2 sm:grid-cols-4 gap-4">
      <div class="card p-4">
        <p class="text-xs text-gray-500">Collected on invoices</p>
        <p class="text-lg font-bold text-green-600 mt-1">{{ formatTotals(summary.collected) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs text-gray-500">Outstanding</p>
        <p class="text-lg font-bold text-amber-600 mt-1">{{ formatTotals(summary.outstanding) }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs text-gray-500">Pending / Paid</p>
        <p class="text-2xl font-bold text-gray-800 mt-1">{{ summary.pending }} / {{ summary.paid }}</p>
      </div>
      <div class="card p-4">
        <p class="text-xs text-gray-500">Overdue</p>
        <p class="text-2xl font-bold text-red-600 mt-1">{{ summary.overdue }}</p>
      </div>
    </div>

    <!-- Filters -->
    <div class="flex flex-wrap gap-3">
      <input v-model="filters.username" type="text" class="form-input w-48" placeholder="Search username..." @input="debouncedLoad" />
      <select v-model="filters.status" @change="page = 1; loadInvoices()" class="form-input w-40">
        <option value="">All Status</option>
        <option value="open">Open (pending + overdue)</option>
        <option value="pending">Pending</option>
        <option value="overdue">Overdue</option>
        <option value="paid">Paid</option>
        <option value="cancelled">Cancelled</option>
      </select>
    </div>

    <!-- Table -->
    <div class="card p-0 overflow-hidden">
      <div class="table-container rounded-none border-0">
        <table class="table">
          <thead>
            <tr>
              <th>Invoice #</th>
              <th>Username</th>
              <th>Plan</th>
              <th>Amount</th>
              <th>Paid</th>
              <th>Status</th>
              <th>Due Date</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading">
              <td colspan="8" class="text-center py-12 text-gray-400">Loading…</td>
            </tr>
            <tr v-else-if="!invoices.length">
              <td colspan="8" class="text-center py-12 text-gray-400">No invoices found</td>
            </tr>
            <tr v-for="inv in invoices" :key="inv.id">
              <td class="font-mono text-xs text-gray-700">{{ inv.invoice_number }}</td>
              <td class="font-medium">{{ inv.username }}</td>
              <td class="text-gray-500 text-sm">
                {{ inv.plan_name || '—' }}
                <span v-if="inv.service_applied && inv.status !== 'paid'" class="badge-blue ml-1" title="The period was given before payment">started</span>
              </td>
              <td class="font-semibold whitespace-nowrap">
                {{ money(inv.amount, inv.currency) }}
                <span v-if="inv.discount_amount > 0" class="block text-xs font-normal text-green-600">
                  −{{ money(inv.discount_amount) }} {{ inv.promo_code || '' }}
                </span>
              </td>
              <td class="text-sm whitespace-nowrap">{{ money(inv.amount_paid) }}</td>
              <td>
                <span class="badge" :class="statusClass(inv.status)">{{ inv.status }}</span>
                <span v-if="inv.paid_at" class="block text-xs text-gray-400">{{ formatDate(inv.paid_at) }}</span>
              </td>
              <td class="text-xs text-gray-500">{{ inv.due_date || '—' }}</td>
              <td>
                <div class="flex flex-wrap gap-1" v-if="authStore.isAdmin">
                  <button v-if="isOpen(inv)" @click="payFor(inv)" class="text-xs px-2 py-1 rounded bg-green-100 text-green-700 hover:bg-green-200">Record payment</button>
                  <button v-if="isOpen(inv) && mpesaEnabled && inv.currency === 'KES'" @click="promptFor = inv" class="text-xs px-2 py-1 rounded bg-emerald-600 text-white hover:bg-emerald-700">M-Pesa prompt</button>
                  <button v-if="inv.status === 'pending'" @click="setStatus(inv, 'overdue')" class="text-xs px-2 py-1 rounded bg-orange-100 text-orange-700 hover:bg-orange-200">Overdue</button>
                  <button v-if="isOpen(inv) && inv.amount_paid === 0" @click="cancel(inv)" class="text-xs px-2 py-1 rounded bg-gray-100 text-gray-700 hover:bg-gray-200">Cancel</button>
                  <button v-if="inv.amount_paid === 0 && inv.status !== 'paid'" @click="remove(inv)" class="text-xs px-2 py-1 rounded bg-red-100 text-red-600 hover:bg-red-200">Delete</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Pagination -->
    <div class="flex items-center justify-between text-sm text-gray-500">
      <span>{{ total }} invoices</span>
      <div class="flex gap-2">
        <button @click="page--; loadInvoices()" :disabled="page <= 1" class="btn-secondary px-3 py-1 text-xs">Prev</button>
        <span class="px-2 py-1">Page {{ page }}</span>
        <button @click="page++; loadInvoices()" :disabled="page * 20 >= total" class="btn-secondary px-3 py-1 text-xs">Next</button>
      </div>
    </div>

    <!-- Create Modal -->
    <div v-if="showModal" class="modal-overlay" @click.self="showModal = false">
      <div class="modal max-w-md">
        <div class="modal-header">
          <h3 class="text-lg font-semibold">New Invoice</h3>
          <button @click="showModal = false" class="text-gray-400 hover:text-gray-600">
            <XMarkIcon class="w-5 h-5" />
          </button>
        </div>
        <form @submit.prevent="create" class="modal-body space-y-4">
          <div>
            <label class="form-label">Username <span class="text-red-500">*</span></label>
            <input v-model.trim="form.username" type="text" class="form-input" required />
          </div>
          <div>
            <label class="form-label">Plan</label>
            <select v-model="form.plan_id" class="form-input" @change="onPlanChange">
              <option :value="null">No plan (one-off charge)</option>
              <option v-for="p in plans" :key="p.id" :value="p.id">{{ p.name }} · {{ money(p.price, p.currency) }} / {{ p.validity_days }} days</option>
            </select>
            <p class="text-xs text-gray-500 mt-1">
              With a plan, paying the invoice adds the plan's days to the customer. To put a customer on a plan,
              use Assign Plan on the Users page instead.
            </p>
          </div>
          <div class="grid grid-cols-2 gap-4">
            <div>
              <label class="form-label">Amount</label>
              <input v-model.number="form.amount" type="number" min="0" step="0.01" class="form-input" :placeholder="form.plan_id ? 'plan price' : '0.00'" />
            </div>
            <div>
              <label class="form-label">Currency</label>
              <input v-model.trim="form.currency" type="text" class="form-input uppercase" maxlength="3" placeholder="default" />
            </div>
          </div>
          <div class="grid grid-cols-2 gap-4">
            <div>
              <label class="form-label">Due Date</label>
              <input v-model="form.due_date" type="date" class="form-input" />
            </div>
            <div>
              <label class="form-label">Promo code</label>
              <input v-model.trim="form.promo_code" type="text" class="form-input uppercase" />
            </div>
          </div>
          <div>
            <label class="form-label">Notes</label>
            <input v-model="form.notes" type="text" class="form-input" placeholder="Optional notes" />
          </div>
          <div class="flex justify-end gap-3 pt-2">
            <button type="button" @click="showModal = false" class="btn-secondary">Cancel</button>
            <button type="submit" class="btn-primary" :disabled="saving">
              <span v-if="saving" class="w-4 h-4 border-2 border-white/30 border-t-white rounded-full spinner"></span>
              Create Invoice
            </button>
          </div>
        </form>
      </div>
    </div>

    <RecordPaymentModal v-if="payingInvoice" :invoice="payingInvoice" @close="payingInvoice = null" @saved="loadInvoices" />
    <MpesaPromptModal v-if="promptFor" :invoice="promptFor" @close="promptFor = null" @saved="loadInvoices" />
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { useToast } from 'vue-toastification'
import { useAuthStore } from '@/store/auth'
import { billingAPI, plansAPI, mpesaAPI } from '@/api'
import { format } from 'date-fns'
import { serverDate } from '../utils/time'
import { PlusIcon, XMarkIcon } from '@heroicons/vue/24/outline'
import RecordPaymentModal from '@/components/billing/RecordPaymentModal.vue'
import MpesaPromptModal from '@/components/billing/MpesaPromptModal.vue'
import { money, formatTotals, apiError } from '@/utils/money'

const toast = useToast()
const authStore = useAuthStore()
const loading = ref(false)
const saving = ref(false)
const showModal = ref(false)
const invoices = ref([])
const plans = ref([])
const total = ref(0)
const page = ref(1)
const payingInvoice = ref(null)
const promptFor = ref(null)
const mpesaEnabled = ref(false)
mpesaAPI.status().then(({ data }) => { mpesaEnabled.value = !!data.enabled }).catch(() => {})
let searchTimer

const summary = reactive({ collected: [], outstanding: [], paid: 0, pending: 0, overdue: 0 })
const filters = reactive({ username: '', status: '' })
const emptyForm = () => ({ username: '', plan_id: null, amount: null, currency: '', due_date: '', promo_code: '', notes: '' })
const form = reactive(emptyForm())

function statusClass(s) {
  return { paid: 'badge-green', pending: 'badge-yellow', overdue: 'badge-red', cancelled: 'badge-gray' }[s] || 'badge-gray'
}
const isOpen = (inv) => inv.status === 'pending' || inv.status === 'overdue'

function formatDate(d) {
  if (!d) return '—'
  try { return format(serverDate(d), 'MMM d, yyyy') } catch { return d }
}

async function loadInvoices() {
  loading.value = true
  try {
    const { data } = await billingAPI.list({ page: page.value, ...filters })
    invoices.value = data.data || []
    total.value = data.total || 0
    if (data.summary) Object.assign(summary, data.summary)
  } catch (err) {
    toast.error(apiError(err, 'Failed to load invoices'))
  } finally { loading.value = false }
}

function debouncedLoad() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => { page.value = 1; loadInvoices() }, 400)
}

function onPlanChange() {
  const p = plans.value.find((x) => x.id === form.plan_id)
  if (p) { form.amount = p.price; form.currency = p.currency }
}

async function openCreate() {
  Object.assign(form, emptyForm())
  showModal.value = true
  if (!plans.value.length) {
    try { const { data } = await plansAPI.list(); plans.value = (data.data || []).filter((p) => p.is_active) } catch { /* optional */ }
  }
}

async function create() {
  saving.value = true
  try {
    const payload = { ...form }
    if (!payload.amount) delete payload.amount
    const { data } = await billingAPI.create(payload)
    toast.success(`Invoice ${data.invoice_number} created`)
    showModal.value = false
    loadInvoices()
  } catch (err) {
    toast.error(apiError(err, 'Failed to create invoice'))
  } finally { saving.value = false }
}

function payFor(inv) { payingInvoice.value = inv }

async function setStatus(inv, status) {
  try {
    await billingAPI.update(inv.id, { status })
    loadInvoices()
  } catch (err) { toast.error(apiError(err)) }
}

async function cancel(inv) {
  if (!confirm(`Cancel invoice ${inv.invoice_number}? It stays on record but is no longer owed.`)) return
  await setStatus(inv, 'cancelled')
}

async function remove(inv) {
  if (!confirm(`Delete invoice ${inv.invoice_number}? This cannot be undone.`)) return
  try {
    await billingAPI.delete(inv.id)
    toast.success('Invoice deleted')
    loadInvoices()
  } catch (err) { toast.error(apiError(err)) }
}

onMounted(loadInvoices)
</script>
