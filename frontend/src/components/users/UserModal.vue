<template>
  <div class="modal-overlay" @click.self="$emit('close')">
    <transition name="slide-up" appear>
      <div class="modal">
        <div class="modal-header">
          <h2 class="text-lg font-semibold text-gray-900">
            {{ isEditing ? 'Edit RADIUS User' : 'Add RADIUS User' }}
          </h2>
          <button @click="$emit('close')" class="p-1.5 hover:bg-gray-100 rounded-lg">
            <XMarkIcon class="w-5 h-5 text-gray-500" />
          </button>
        </div>

        <form @submit.prevent="submit" class="modal-body space-y-4">
          <div class="grid grid-cols-2 gap-4">
            <div>
              <label class="form-label">Username *</label>
              <input v-model.trim="form.username" :disabled="isEditing" type="text" class="form-input" required
                     pattern="[^\s'&quot;\\]+" title="No spaces or quotes" />
            </div>
            <div v-if="!isEditing">
              <label class="form-label">Password *</label>
              <div class="relative">
                <input v-model="form.password" :type="showPw ? 'text' : 'password'" class="form-input pr-9"
                       required minlength="6" />
                <button type="button" class="absolute right-2 top-2 text-gray-400" @click="showPw = !showPw">
                  <EyeIcon v-if="!showPw" class="w-4 h-4" />
                  <EyeSlashIcon v-else class="w-4 h-4" />
                </button>
              </div>
              <p class="text-xs text-gray-500 mt-1">At least 6 characters. The customer types this on the Wi-Fi / PPPoE login.</p>
            </div>
          </div>

          <div class="grid grid-cols-2 gap-4">
            <div>
              <label class="form-label">Full Name</label>
              <input v-model="form.full_name" type="text" class="form-input" />
            </div>
            <div>
              <label class="form-label">Phone</label>
              <input v-model.trim="form.phone" type="tel" class="form-input" placeholder="+254…" maxlength="30" />
            </div>
          </div>

          <div class="grid grid-cols-2 gap-4">
            <div>
              <label class="form-label">Email</label>
              <input v-model.trim="form.email" type="email" class="form-input" />
            </div>
            <div>
              <label class="form-label">Department / Area</label>
              <input v-model="form.department" type="text" class="form-input" />
            </div>
          </div>

          <!-- New customer: plan or a manual expiry -->
          <div v-if="!isEditing" class="rounded-xl border border-gray-200 p-3 space-y-3">
            <div>
              <label class="form-label">Plan</label>
              <select v-model="form.plan_id" class="form-input">
                <option :value="null">No plan</option>
                <option v-for="p in plans" :key="p.id" :value="p.id">
                  {{ p.name }} · {{ p.price > 0 ? money(p.price, p.currency) : 'free' }} / {{ p.validity_days }} days
                </option>
              </select>
            </div>
            <template v-if="selectedPlan && selectedPlan.price > 0">
              <label class="flex items-start gap-2 text-sm text-gray-700">
                <input v-model="form.start_now" type="checkbox" class="rounded mt-0.5" />
                <span>Start service now, before payment (postpaid). Otherwise the customer is offline until the invoice is paid.</span>
              </label>
              <div>
                <label class="form-label">Promo code</label>
                <input v-model.trim="form.promo_code" type="text" class="form-input uppercase" />
              </div>
              <div v-if="!form.start_now">
                <label class="form-label">Free trial days (optional)</label>
                <input v-model.number="form.trial_days" type="number" min="0" max="90" class="form-input" placeholder="Blank = default from Settings" />
                <p class="text-xs text-gray-500 mt-1">Online this many days before the first payment (0 = none).</p>
              </div>
            </template>
            <div v-if="!form.plan_id">
              <label class="form-label">Paid until (optional)</label>
              <input v-model="form.account_expiry" type="date" class="form-input" />
              <p class="text-xs text-gray-500 mt-1">Empty = never expires.</p>
            </div>
          </div>

          <div v-else>
            <label class="form-label">Paid until</label>
            <input v-model="form.account_expiry" type="date" class="form-input" />
            <p class="text-xs text-amber-600 mt-1">
              Normally moved by payments. Changing it here gives or takes days without an invoice; empty = never expires.
            </p>
          </div>

          <div>
            <label class="form-label">Grace days (optional)</label>
            <input v-model.number="form.grace_days" type="number" min="0" max="60" class="form-input" placeholder="Blank = system default" />
            <p class="text-xs text-gray-500 mt-1">
              Days this customer stays online after their paid-until date while waiting to pay. Give extra days
              here when a customer asks; leave blank to use the default from Settings.
            </p>
          </div>

          <div>
            <label class="form-label">
              Device Limit
              <span class="ml-2 font-semibold text-blue-600">{{ form.device_limit }}</span>
            </label>
            <input v-model.number="form.device_limit" type="range" min="1" max="20" class="w-full accent-blue-600" />
            <p class="text-xs text-gray-400">Devices online at the same time. A plan's device count overrides this.</p>
          </div>

          <div>
            <label class="form-label">Notes</label>
            <textarea v-model="form.notes" rows="2" class="form-input" placeholder="Location, router, anything staff should know"></textarea>
          </div>

          <div v-if="error" class="p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-600">
            {{ error }}
          </div>
          <button type="submit" class="hidden"></button>
        </form>

        <div class="modal-footer">
          <button @click="$emit('close')" class="btn-secondary">Cancel</button>
          <button @click="submit" class="btn-primary" :disabled="saving">
            <span v-if="saving" class="w-4 h-4 border-2 border-white/30 border-t-white rounded-full spinner"></span>
            {{ isEditing ? 'Save Changes' : 'Create' }}
          </button>
        </div>
      </div>
    </transition>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted } from 'vue'
import { radiusUsersAPI, plansAPI } from '@/api'
import { XMarkIcon, EyeIcon, EyeSlashIcon } from '@heroicons/vue/24/outline'
import { useToast } from 'vue-toastification'
import { money, apiError } from '@/utils/money'

const props = defineProps({ user: Object })
const emit = defineEmits(['close', 'saved'])
const toast = useToast()

const isEditing = computed(() => !!props.user?.id)
const saving = ref(false)
const showPw = ref(false)
const error = ref('')
const plans = ref([])
const selectedPlan = computed(() => plans.value.find((p) => p.id === form.value.plan_id))

const blank = () => ({
  username: '', password: '', full_name: '', email: '', phone: '', department: '', notes: '',
  device_limit: 1, account_expiry: '', grace_days: null, trial_days: null, plan_id: null, start_now: false, promo_code: '',
})
const form = ref(blank())

watch(() => props.user, (u) => {
  if (u) {
    form.value = {
      ...blank(),
      username: u.username || '',
      full_name: u.full_name || '',
      email: u.email || '',
      phone: u.phone || '',
      department: u.department || '',
      notes: u.notes || '',
      device_limit: u.device_limit || 1,
      account_expiry: u.account_expiry ? u.account_expiry.slice(0, 10) : '',
      grace_days: u.grace_days ?? null,
    }
  }
}, { immediate: true })

onMounted(async () => {
  if (isEditing.value) return
  try { plans.value = ((await plansAPI.list()).data.data || []).filter((p) => p.is_active) } catch { /* optional */ }
})

async function submit() {
  error.value = ''
  saving.value = true
  try {
    const f = form.value
    if (isEditing.value) {
      // Expiry and grace are sent only when changed: the user row may be stale, and sending it
      // back would undo a payment made since the list was loaded.
      const was = props.user.account_expiry ? props.user.account_expiry.slice(0, 10) : ''
      const graceWas = props.user.grace_days ?? null
      const grace = f.grace_days === '' ? null : f.grace_days
      await radiusUsersAPI.update(props.user.id, {
        full_name: f.full_name, email: f.email, phone: f.phone, department: f.department, notes: f.notes,
        device_limit: f.device_limit,
        ...((f.account_expiry || '') !== was ? { account_expiry: f.account_expiry || '', account_expiry_was: was } : {}), // "" clears the expiry
        ...(grace === graceWas ? {} : grace === null ? { clear_grace_days: true } : { grace_days: grace }),
      })
      toast.success('User updated')
      emit('saved')
    } else {
      const payload = { ...f }
      if (payload.plan_id || !payload.account_expiry) delete payload.account_expiry
      if (!payload.plan_id) { delete payload.start_now; delete payload.promo_code }
      if (payload.grace_days === null || payload.grace_days === '') delete payload.grace_days
      if (payload.trial_days === null || payload.trial_days === '') delete payload.trial_days
      const { data } = await radiusUsersAPI.create(payload)
      toast.success(data.message || 'User created')
      emit('saved', data)
    }
  } catch (err) {
    error.value = apiError(err, 'Failed to save user')
  } finally {
    saving.value = false
  }
}
</script>
