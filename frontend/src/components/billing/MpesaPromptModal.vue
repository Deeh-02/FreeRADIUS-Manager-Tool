<template>
  <div class="modal-overlay" @click.self="$emit('close')">
    <div class="modal max-w-md">
      <div class="modal-header">
        <h3 class="text-lg font-semibold">Send M-Pesa prompt</h3>
        <button @click="$emit('close')" class="text-gray-400 hover:text-gray-600"><XMarkIcon class="w-5 h-5" /></button>
      </div>

      <form v-if="!promptId || state === 'failed'" @submit.prevent="send" class="modal-body space-y-4">
        <p class="text-sm text-gray-600">
          {{ invoice.invoice_number }} · {{ invoice.username }} ·
          <b>{{ money(invoice.amount - (invoice.amount_paid || 0), invoice.currency) }}</b> due.
          The customer's phone shows a prompt; when they enter their PIN the payment is recorded and
          they are online, with no one typing anything.
        </p>
        <div>
          <label class="form-label">Customer's M-Pesa number</label>
          <input v-model.trim="phone" type="tel" class="form-input" placeholder="07XX XXX XXX" required />
        </div>
        <p v-if="error" class="text-sm text-red-600">{{ error }}</p>
        <div class="flex justify-end gap-3">
          <button type="button" @click="$emit('close')" class="btn-secondary">Cancel</button>
          <button type="submit" class="btn-primary" :disabled="sending">{{ sending ? 'Sending…' : 'Send prompt' }}</button>
        </div>
      </form>

      <div v-else class="modal-body space-y-3 text-center">
        <template v-if="state === 'paid'">
          <CheckCircleIcon class="w-12 h-12 text-green-500 mx-auto" />
          <p class="font-semibold">Paid: KES {{ amount }}, M-Pesa {{ receipt || '(code to follow)' }}</p>
          <p v-if="expiry" class="text-sm text-gray-500">{{ invoice.username }} is paid up to {{ expiry }}.</p>
          <button @click="$emit('close')" class="btn-primary">Done</button>
        </template>
        <template v-else>
          <span class="inline-block w-8 h-8 border-4 border-green-200 border-t-green-600 rounded-full spinner"></span>
          <p class="font-medium">Waiting for {{ sentTo }} to enter their PIN (KES {{ amount }})…</p>
          <p class="text-xs text-gray-500">You can close this; the payment is recorded when Safaricom confirms it.</p>
        </template>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { XMarkIcon, CheckCircleIcon } from '@heroicons/vue/24/outline'
import { mpesaAPI, radiusUsersAPI } from '@/api'
import { money, apiError } from '@/utils/money'

const props = defineProps({ invoice: { type: Object, required: true } })
const emit = defineEmits(['close', 'saved'])
const phone = ref(props.invoice.phone || '')
const sending = ref(false)
const error = ref('')
const promptId = ref(null)
const state = ref('')
const amount = ref(0)
const sentTo = ref('')
const receipt = ref('')
const expiry = ref('')
let timer = null

async function send() {
  sending.value = true
  error.value = ''
  try {
    const { data } = await mpesaAPI.prompt(props.invoice.id, phone.value)
    promptId.value = data.id
    amount.value = data.amount
    sentTo.value = data.phone
    state.value = 'pending'
    poll()
  } catch (err) {
    error.value = apiError(err, 'Could not send the prompt')
  } finally {
    sending.value = false
  }
}

function poll(tries = 0) {
  timer = setTimeout(async () => {
    try {
      const { data } = await mpesaAPI.promptStatus(promptId.value)
      if (data.status === 'paid') {
        state.value = 'paid'
        receipt.value = data.mpesa_receipt
        expiry.value = data.account_expiry
        emit('saved')
        return
      }
      if (data.status === 'failed') {
        state.value = 'failed'
        error.value = data.result_desc || 'The customer did not complete the payment'
        return
      }
    } catch { /* keep polling */ }
    if (tries < 60) poll(tries + 1)
  }, 3000)
}

// The number on the customer's record, if staff have one.
onMounted(async () => {
  if (phone.value) return
  try {
    const { data } = await radiusUsersAPI.list({ search: props.invoice.username, limit: 20 })
    const u = (data.data || []).find((x) => x.username === props.invoice.username)
    if (u?.phone && !phone.value) phone.value = u.phone
  } catch { /* staff type it */ }
})

onBeforeUnmount(() => clearTimeout(timer))
</script>
