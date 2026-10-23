<template>
  <div v-if="enabled && list.length" class="card border border-amber-200">
    <div class="flex items-center justify-between mb-2">
      <h3 class="font-semibold text-gray-900 text-sm">
        M-Pesa payments waiting for a customer ({{ count }} · KES {{ amount.toFixed(2) }})
      </h3>
      <button @click="load" class="text-xs text-blue-600">Refresh</button>
    </div>
    <p class="text-xs text-gray-500 mb-3">
      The customer paid the paybill/till with an account number that matches nobody. The money has
      been received; give it to the right customer so their service starts.
    </p>
    <table class="w-full text-sm">
      <thead>
        <tr class="text-xs text-gray-400 border-b">
          <th class="text-left py-1">M-Pesa code</th><th class="text-left">Account typed</th>
          <th class="text-left">Payer</th><th class="text-right">Amount</th><th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="p in list" :key="p.id" class="border-b border-gray-50">
          <td class="py-1.5 font-mono text-xs">{{ p.trans_id }}<span class="block text-gray-400">{{ p.created_at?.slice(0, 16).replace('T', ' ') }}</span></td>
          <td class="text-gray-700">{{ p.bill_ref || '—' }}</td>
          <td class="text-gray-500 text-xs">{{ p.payer_name }} {{ p.msisdn }}</td>
          <td class="text-right font-medium">KES {{ p.amount.toFixed(2) }}</td>
          <td class="text-right">
            <form v-if="canAllocate" @submit.prevent="allocate(p)" class="flex gap-1 justify-end">
              <input v-model.trim="target[p.id]" placeholder="username" class="form-input text-xs w-28 py-1" required />
              <button class="text-xs px-2 py-1 rounded bg-green-600 text-white" :disabled="busy === p.id">Give</button>
            </form>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { useToast } from 'vue-toastification'
import { mpesaAPI } from '@/api'
import { apiError } from '@/utils/money'

defineProps({ canAllocate: { type: Boolean, default: false } })
const emit = defineEmits(['allocated'])
const toast = useToast()
const enabled = ref(false)
const list = ref([])
const count = ref(0)
const amount = ref(0)
const target = reactive({})
const busy = ref(null)

async function load() {
  try {
    const { data } = await mpesaAPI.c2b({ status: 'unmatched', limit: 50 })
    list.value = data.data || []
    count.value = data.unmatched
    amount.value = data.unmatched_amount
  } catch { list.value = [] }
}

async function allocate(p) {
  busy.value = p.id
  try {
    const { data } = await mpesaAPI.allocate(p.id, target[p.id])
    toast.success(data.message)
    await load()
    emit('allocated')
  } catch (err) {
    toast.error(apiError(err, 'Could not allocate'))
  } finally { busy.value = null }
}

onMounted(async () => {
  try { enabled.value = !!(await mpesaAPI.status()).data.enabled } catch { enabled.value = false }
  if (enabled.value) load()
})
</script>
