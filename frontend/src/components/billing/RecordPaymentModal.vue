<template>
  <div class="modal-overlay" @click.self="$emit('close')">
    <div class="modal max-w-md">
      <div class="modal-header">
        <h3 class="text-lg font-semibold">Record Payment</h3>
        <button @click="$emit('close')" class="text-gray-400 hover:text-gray-600"><XMarkIcon class="w-5 h-5" /></button>
      </div>

      <form v-if="!result" @submit.prevent="save" class="modal-body space-y-4">
        <div>
          <label class="form-label">Invoice</label>
          <select v-model="invoiceId" class="form-input" @change="onInvoiceChange">
            <option :value="null">No invoice (money on account)</option>
            <option v-for="inv in openInvoices" :key="inv.id" :value="inv.id">
              {{ inv.invoice_number }} · {{ inv.username }} · {{ money(inv.amount - inv.amount_paid, inv.currency) }} due
            </option>
          </select>
          <p class="text-xs text-gray-500 mt-1">
            Paying an invoice in full marks it paid and, for a plan invoice, adds the plan period to the customer.
          </p>
        </div>

        <div v-if="!invoiceId">
          <label class="form-label">Customer username <span class="text-red-500">*</span></label>
          <input v-model.trim="form.username" type="text" class="form-input" required />
          <p class="text-xs text-amber-600 mt-1">Without an invoice the payment is recorded only; it does not extend service.</p>
        </div>

        <div class="grid grid-cols-3 gap-3">
          <div class="col-span-2">
            <label class="form-label">Amount <span class="text-red-500">*</span></label>
            <input v-model.number="form.amount" type="number" min="0.01" step="0.01" class="form-input" required />
          </div>
          <div>
            <label class="form-label">Currency</label>
            <input v-model.trim="form.currency" type="text" maxlength="3" class="form-input uppercase"
                   :disabled="!!invoiceId" placeholder="default" />
          </div>
        </div>

        <div class="grid grid-cols-2 gap-3">
          <div>
            <label class="form-label">Method</label>
            <select v-model="form.payment_method" class="form-input">
              <option value="cash">Cash</option>
              <option value="mobile_money">Mobile money</option>
              <option value="bank_transfer">Bank transfer</option>
              <option value="card">Card</option>
              <option value="online">Online</option>
              <option value="other">Other</option>
            </select>
          </div>
          <div>
            <label class="form-label">Reference</label>
            <input v-model.trim="form.gateway_ref" type="text" class="form-input" placeholder="e.g. M-Pesa code" />
          </div>
        </div>
        <p class="text-xs text-gray-500 -mt-2">A reference can only be used once, so the same transfer cannot be recorded twice.</p>

        <div>
          <label class="form-label">Notes</label>
          <input v-model="form.notes" type="text" class="form-input" />
        </div>

        <div class="flex justify-end gap-3 pt-2">
          <button type="button" @click="$emit('close')" class="btn-secondary">Cancel</button>
          <button type="submit" class="btn-primary" :disabled="saving">
            <span v-if="saving" class="w-4 h-4 border-2 border-white/30 border-t-white rounded-full spinner"></span>
            Record Payment
          </button>
        </div>
      </form>

      <div v-else class="modal-body space-y-4 text-center">
        <CheckCircleIcon class="w-12 h-12 text-green-500 mx-auto" />
        <p class="font-semibold">{{ result.message }}</p>
        <p class="text-sm text-gray-500">Receipt <span class="font-mono">{{ result.receipt_number }}</span></p>
        <div class="flex justify-center gap-3">
          <button @click="showReceipt" class="btn-secondary">View receipt</button>
          <button @click="$emit('close')" class="btn-primary">Done</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { useToast } from 'vue-toastification'
import { XMarkIcon, CheckCircleIcon } from '@heroicons/vue/24/outline'
import { billingAPI, paymentsAPI, openReceipt } from '@/api'
import { money, apiError } from '@/utils/money'
import { newIdempotencyKey } from '@/utils/idempotency'

// invoice: optional invoice to pre-select (from the Billing page).
const props = defineProps({ invoice: { type: Object, default: null } })
const emit = defineEmits(['close', 'saved'])
const toast = useToast()

const openInvoices = ref([])
const invoiceId = ref(props.invoice?.id ?? null)
const saving = ref(false)
const result = ref(null)
// One key for this dialog: pressing Save twice, or the browser retrying after a timeout, can
// never record the money twice.
const idempotencyKey = newIdempotencyKey()
const form = reactive({ username: '', amount: null, currency: '', payment_method: 'cash', gateway_ref: '', notes: '' })

function onInvoiceChange() {
  const inv = openInvoices.value.find((i) => i.id === invoiceId.value)
  if (inv) {
    form.amount = Math.round((inv.amount - inv.amount_paid) * 100) / 100
    form.currency = inv.currency
    form.username = inv.username
  }
}

onMounted(async () => {
  try {
    const { data } = await billingAPI.list({ status: 'open', limit: 100 })
    openInvoices.value = data.data || []
  } catch { /* the list is optional */ }
  if (props.invoice && !openInvoices.value.some((i) => i.id === props.invoice.id)) {
    let inv = props.invoice
    if (!inv.invoice_number) {
      try { inv = (await billingAPI.get(inv.id)).data.invoice } catch { inv = null }
    }
    if (inv) openInvoices.value.unshift(inv)
    else invoiceId.value = null
  }
  onInvoiceChange()
})

async function save() {
  saving.value = true
  try {
    const payload = { ...form, invoice_id: invoiceId.value }
    if (invoiceId.value) delete payload.currency
    const { data } = await paymentsAPI.create(payload, { headers: { 'Idempotency-Key': idempotencyKey } })
    result.value = data
    emit('saved', data)
  } catch (err) {
    toast.error(apiError(err, 'Failed to record payment'))
  } finally {
    saving.value = false
  }
}

async function showReceipt() {
  try { await openReceipt(result.value.id) } catch (err) { toast.error(apiError(err, 'Could not load receipt')) }
}
</script>
