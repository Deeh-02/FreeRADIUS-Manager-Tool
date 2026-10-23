<template>
  <div class="modal-overlay" @click.self="$emit('close')">
    <div class="modal max-w-md">
      <div class="modal-header">
        <h3 class="text-lg font-semibold">Plan for {{ user.username }}</h3>
        <button @click="$emit('close')" class="text-gray-400 hover:text-gray-600"><XMarkIcon class="w-5 h-5" /></button>
      </div>

      <form v-if="!result" @submit.prevent="save" class="modal-body space-y-4">
        <p v-if="user.plan_name" class="text-sm text-gray-600">
          Current plan: <strong>{{ user.plan_name }}</strong>
          <span v-if="user.account_expiry"> · paid until {{ user.account_expiry.slice(0, 10) }}</span>
        </p>
        <div>
          <label class="form-label">Plan</label>
          <select v-model="planId" class="form-input" required>
            <option :value="null" disabled>Choose a plan…</option>
            <option v-for="p in plans" :key="p.id" :value="p.id">
              {{ p.name }} · {{ p.price > 0 ? money(p.price, p.currency) : 'free' }} / {{ p.validity_days }} days
            </option>
          </select>
        </div>
        <div v-if="plan" class="text-xs text-gray-500 bg-gray-50 rounded-lg p-3 space-y-1">
          <p>Limits applied now: {{ plan.max_devices }} device(s){{ plan.data_limit_mb ? `, ${plan.data_limit_mb} MB per period` : '' }}{{ plan.bandwidth_name ? `, speed ${plan.bandwidth_name}` : '' }}.</p>
          <p v-if="plan.price > 0">An invoice for {{ money(plan.price, plan.currency) }} is raised. Paying it adds {{ plan.validity_days }} days
            (from the paid-until date if still running, otherwise from today).</p>
          <p v-else>Free plan: {{ plan.validity_days }} days are added straight away.</p>
        </div>
        <template v-if="plan && plan.price > 0">
          <label class="flex items-start gap-2 text-sm text-gray-700">
            <input v-model="startNow" type="checkbox" class="rounded mt-0.5" />
            <span>Start the period now, before payment (postpaid). If unchecked and the customer has no paid days left, they are offline until the invoice is paid.</span>
          </label>
          <div>
            <label class="form-label">Promo code</label>
            <input v-model.trim="promo" type="text" class="form-input uppercase" />
          </div>
          <div v-if="!startNow">
            <label class="form-label">Free trial days (optional)</label>
            <input v-model.number="trialDays" type="number" min="0" max="90" class="form-input" placeholder="Blank = default from Settings" />
            <p class="text-xs text-gray-500 mt-1">
              For a customer who has never paid: online this many days before the first payment (0 = none).
              Each customer gets one trial.
            </p>
          </div>
        </template>
        <div class="flex justify-between gap-3 pt-2">
          <button v-if="user.plan_id" type="button" @click="unlink" class="text-sm text-red-600 hover:underline" :disabled="saving">Remove plan</button>
          <span v-else></span>
          <div class="flex gap-3">
            <button type="button" @click="$emit('close')" class="btn-secondary">Cancel</button>
            <button type="submit" class="btn-primary" :disabled="saving || !planId">Assign</button>
          </div>
        </div>
      </form>

      <div v-else class="modal-body space-y-4 text-center">
        <CheckCircleIcon class="w-12 h-12 text-green-500 mx-auto" />
        <p class="font-medium">{{ result.message }}</p>
        <div class="flex justify-center gap-3">
          <button v-if="result.invoice_id" @click="$emit('pay', { id: result.invoice_id })" class="btn-secondary">Record payment now</button>
          <button @click="$emit('close')" class="btn-primary">Done</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { useToast } from 'vue-toastification'
import { XMarkIcon, CheckCircleIcon } from '@heroicons/vue/24/outline'
import { plansAPI } from '@/api'
import { money, apiError } from '@/utils/money'

const props = defineProps({ user: { type: Object, required: true } })
const emit = defineEmits(['close', 'saved', 'pay'])
const toast = useToast()

const plans = ref([])
const planId = ref(props.user.plan_id ?? null)
const startNow = ref(false)
const promo = ref('')
const trialDays = ref(null)
const saving = ref(false)
const result = ref(null)
const plan = computed(() => plans.value.find((p) => p.id === planId.value))

onMounted(async () => {
  try { plans.value = ((await plansAPI.list()).data.data || []).filter((p) => p.is_active || p.id === props.user.plan_id) }
  catch (err) { toast.error(apiError(err, 'Could not load plans')) }
})

async function save() {
  saving.value = true
  try {
    const { data } = await plansAPI.assignToUser(props.user.id, {
      plan_id: planId.value, start_now: startNow.value, promo_code: promo.value,
      ...(trialDays.value === null || trialDays.value === '' ? {} : { trial_days: trialDays.value }),
    })
    result.value = data
    emit('saved')
  } catch (err) {
    toast.error(apiError(err, 'Failed to assign plan'))
  } finally { saving.value = false }
}

async function unlink() {
  if (!confirm(`Remove the plan from ${props.user.username}? Limits already applied stay until changed.`)) return
  saving.value = true
  try {
    const { data } = await plansAPI.assignToUser(props.user.id, { plan_id: null })
    toast.success(data.message)
    emit('saved')
    emit('close')
  } catch (err) {
    toast.error(apiError(err))
  } finally { saving.value = false }
}
</script>
