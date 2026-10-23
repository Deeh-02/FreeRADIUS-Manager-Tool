<template>
  <div class="space-y-5">
    <div class="page-header">
      <div>
        <h1 class="page-title">Payments</h1>
        <p class="text-sm text-gray-500 mt-0.5">Money received. Payments are never deleted; a mistake or refund is reversed.</p>
      </div>
      <button @click="showModal = true" class="btn-primary" v-if="authStore.isAdmin">
        <PlusIcon class="w-4 h-4" />
        Record Payment
      </button>
    </div>

    <!-- Summary cards (admins; never mixing currencies) -->
    <div v-if="summary" class="grid grid-cols-1 sm:grid-cols-3 gap-3">
      <div class="card p-4 flex items-center gap-3">
        <div class="w-9 h-9 bg-green-100 rounded-xl flex items-center justify-center shrink-0">
          <CurrencyDollarIcon class="w-5 h-5 text-green-600" />
        </div>
        <div>
          <p class="text-xs text-gray-500">This month</p>
          <p class="text-lg font-bold text-green-600">{{ formatTotals(summary.this_month) }}</p>
          <p class="text-xs text-gray-400">{{ countOf(summary.this_month) }} payments</p>
        </div>
      </div>
      <div class="card p-4 flex items-center gap-3">
        <div class="w-9 h-9 bg-blue-100 rounded-xl flex items-center justify-center shrink-0">
          <BanknotesIcon class="w-5 h-5 text-blue-600" />
        </div>
        <div>
          <p class="text-xs text-gray-500">All time</p>
          <p class="text-lg font-bold text-blue-600">{{ formatTotals(summary.all_time) }}</p>
          <p class="text-xs text-gray-400">{{ countOf(summary.all_time) }} payments</p>
        </div>
      </div>
      <div class="card p-4 flex items-center gap-3">
        <div class="w-9 h-9 bg-amber-100 rounded-xl flex items-center justify-center shrink-0">
          <ReceiptPercentIcon class="w-5 h-5 text-amber-600" />
        </div>
        <div>
          <p class="text-xs text-gray-500">Outstanding on open invoices</p>
          <p class="text-lg font-bold text-amber-600">{{ formatTotals(summary.outstanding) }}</p>
          <p class="text-xs text-gray-400">{{ countOf(summary.outstanding) }} invoices</p>
        </div>
      </div>
    </div>

    <!-- Method breakdown -->
    <div v-if="summary?.by_method?.length" class="card">
      <h3 class="font-semibold text-gray-900 mb-3 text-sm">Revenue by payment method</h3>
      <div class="flex flex-wrap gap-3">
        <div v-for="m in summary.by_method" :key="m.method + m.currency"
          class="flex items-center gap-2 bg-gray-50 rounded-xl px-3 py-2">
          <span class="text-xs font-medium capitalize">{{ m.method.replace('_',' ') }}</span>
          <span class="text-sm font-bold text-blue-700">{{ money(m.total, m.currency) }}</span>
          <span class="text-xs text-gray-400">({{ m.count }})</span>
        </div>
      </div>
    </div>

    <!-- Filters -->
    <div class="flex items-center gap-3 flex-wrap">
      <input v-model="filterUsername" type="text" class="form-input w-48 text-sm" placeholder="Filter by username…" @input="debouncedLoad" />
      <select v-model="filterMethod" class="form-input w-36 text-sm" @change="load">
        <option value="">All methods</option>
        <option value="cash">Cash</option>
        <option value="bank_transfer">Bank Transfer</option>
        <option value="mobile_money">Mobile Money</option>
        <option value="card">Card</option>
        <option value="online">Online</option>
        <option value="voucher">Voucher</option>
        <option value="other">Other</option>
      </select>
      <select v-model="filterStatus" class="form-input w-36 text-sm" @change="load">
        <option value="">All statuses</option>
        <option value="confirmed">Confirmed</option>
        <option value="reversed">Reversed</option>
      </select>
      <span v-if="filterUsername && totals.length" class="text-sm text-gray-500">
        Confirmed for "{{ filterUsername }}": <strong>{{ formatTotals(totals) }}</strong>
      </span>
    </div>

    <!-- Payments table -->
    <div class="card p-0 overflow-hidden">
      <div class="table-container rounded-none border-0">
        <table class="table">
          <thead>
            <tr>
              <th>Receipt</th>
              <th>Customer</th>
              <th>Invoice</th>
              <th>Amount</th>
              <th>Method</th>
              <th>Status</th>
              <th>Date</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading"><td colspan="8" class="text-center py-8 text-gray-400">Loading…</td></tr>
            <tr v-else-if="!payments.length"><td colspan="8" class="text-center py-8 text-gray-400">No payments found</td></tr>
            <tr v-for="p in payments" :key="p.id" :class="{ 'opacity-60': p.status === 'reversed' }">
              <td><span class="font-mono text-xs">{{ p.receipt_number }}</span></td>
              <td class="text-sm font-medium">{{ p.username || '—' }}</td>
              <td class="text-xs text-gray-500">{{ p.invoice_number || '—' }}</td>
              <td class="whitespace-nowrap">
                <span class="font-bold" :class="p.status === 'reversed' ? 'line-through text-gray-500' : 'text-green-600'">{{ money(p.amount, p.currency) }}</span>
                <span v-if="p.gateway_ref" class="block text-xs text-gray-400 font-mono">{{ p.gateway_ref }}</span>
              </td>
              <td>
                <span class="badge badge-blue text-xs capitalize">{{ p.payment_method?.replace('_',' ') }}</span>
              </td>
              <td>
                <span class="badge" :class="p.status === 'reversed' ? 'badge-red' : 'badge-green'">{{ p.status }}</span>
                <span v-if="p.reversal_reason" class="block text-xs text-gray-400" :title="p.reversal_reason">{{ p.reversal_reason }}</span>
              </td>
              <td class="text-xs text-gray-400">{{ formatDate(p.created_at) }}</td>
              <td>
                <div class="flex gap-1">
                  <button @click="viewReceipt(p)" class="p-1.5 rounded hover:bg-blue-50 text-gray-400 hover:text-blue-600" title="View receipt">
                    <ReceiptPercentIcon class="w-4 h-4" />
                  </button>
                  <button v-if="authStore.isAdmin && p.status === 'confirmed'" @click="reverse(p)"
                    class="p-1.5 rounded hover:bg-red-50 text-gray-400 hover:text-red-500" title="Reverse payment">
                    <ArrowUturnLeftIcon class="w-4 h-4" />
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div class="flex items-center justify-between text-sm text-gray-500">
      <span>{{ total }} payments</span>
      <div class="flex gap-2">
        <button @click="page--; load()" :disabled="page <= 1" class="btn-secondary px-3 py-1 text-xs">Prev</button>
        <span class="px-2 py-1">Page {{ page }}</span>
        <button @click="page++; load()" :disabled="page * 20 >= total" class="btn-secondary px-3 py-1 text-xs">Next</button>
      </div>
    </div>

    <RecordPaymentModal v-if="showModal" @close="showModal = false" @saved="load" />
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { useToast } from 'vue-toastification'
import { useAuthStore } from '@/store/auth'
import { paymentsAPI, openReceipt } from '@/api'
import { format, parseISO } from 'date-fns'
import { PlusIcon, CurrencyDollarIcon, BanknotesIcon, ReceiptPercentIcon, ArrowUturnLeftIcon } from '@heroicons/vue/24/outline'
import RecordPaymentModal from '@/components/billing/RecordPaymentModal.vue'
import { money, formatTotals, apiError } from '@/utils/money'

const toast = useToast()
const authStore = useAuthStore()
const loading = ref(false)
const showModal = ref(false)
const payments = ref([])
const total = ref(0)
const totals = ref([])
const page = ref(1)
const summary = ref(null)
const filterUsername = ref('')
const filterMethod = ref('')
const filterStatus = ref('')
let debounceTimer

function formatDate(d) { try { return format(parseISO(d), 'MMM d, yyyy HH:mm') } catch { return d } }
const countOf = (list) => (list || []).reduce((n, t) => n + (t.count || 0), 0)

async function load() {
  loading.value = true
  try {
    const { data } = await paymentsAPI.list({
      page: page.value, username: filterUsername.value, method: filterMethod.value, status: filterStatus.value,
    })
    payments.value = data.data || []
    total.value = data.total || 0
    totals.value = data.totals || []
  } catch (err) {
    toast.error(apiError(err, 'Failed to load payments'))
  } finally { loading.value = false }
  // The revenue summary is for admins only; operators just do not see the cards.
  if (authStore.isAdmin) {
    try { summary.value = (await paymentsAPI.summary()).data } catch { summary.value = null }
  }
}

function debouncedLoad() {
  clearTimeout(debounceTimer)
  debounceTimer = setTimeout(() => { page.value = 1; load() }, 300)
}

async function viewReceipt(p) {
  try { await openReceipt(p.id) } catch (err) { toast.error(apiError(err, 'Could not load receipt')) }
}

async function reverse(p) {
  const reason = prompt(
    `Reverse payment ${p.receipt_number} (${money(p.amount, p.currency)})?\n` +
    'Its invoice re-opens and any plan days it paid for are taken back.\n\nReason:')
  if (reason === null) return
  try {
    const { data } = await paymentsAPI.reverse(p.id, reason)
    toast.success(data.message || 'Payment reversed')
    load()
  } catch (err) { toast.error(apiError(err, 'Failed to reverse payment')) }
}

onMounted(load)
</script>
