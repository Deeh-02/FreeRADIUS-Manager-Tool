// One customer's money story, clicked through the real screens: create with a plan, pay the
// invoice on the Billing page, edit the customer afterwards (B31: this used to undo the
// payment), reverse the payment on the Payments page. After each step the backend is asked
// what really happened. Needs an empty database (scripts/test-stack.sh start).
import { test, expect } from '@playwright/test'

const API = (process.env.API_PROXY_TARGET || 'http://127.0.0.1:18080') + '/api/v1'
const ADMIN = { username: 'browseradmin', password: 'Browser-test-pass-1!' }
const CUSTOMER = 'browsercust' + Date.now().toString(36)

let token

async function api(request, method, path, data) {
  const res = await request.fetch(API + path, {
    method,
    data,
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  const body = await res.json().catch(() => ({}))
  if (!res.ok()) throw new Error(`${method} ${path}: ${res.status()} ${JSON.stringify(body)}`)
  return body
}

async function customer(request) {
  const body = await api(request, 'GET', `/radius/users?search=${CUSTOMER}`)
  const list = body.data || body.users || []
  return list.find((u) => u.username === CUSTOMER)
}

// The date part of a paid-until value ("2026-11-24T00:00:00Z" → "2026-11-24").
const day = (v) => (v ? String(v).slice(0, 10) : '')

function inDays(n) {
  const d = new Date()
  d.setDate(d.getDate() + n)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

// The input or select right after a form label (the forms do not link labels to inputs).
function field(scope, label) {
  return scope.locator(
    `xpath=.//label[starts-with(normalize-space(.), "${label}")]/following-sibling::*[1]/descendant-or-self::*[self::input or self::select or self::textarea][1]`,
  )
}

test.describe.configure({ mode: 'serial' })

test.beforeAll(async ({ request }) => {
  const status = await api(request, 'GET', '/setup/status')
  if (status.setup_required !== false) {
    await api(request, 'POST', '/setup/complete', {
      setup_token: process.env.SETUP_TOKEN || '',
      organization: { name: 'Browser Test ISP', timezone: 'Africa/Nairobi' },
      admin: { ...ADMIN, email: 'browser@example.com', full_name: 'Browser Admin' },
    })
  }
  token = (await api(request, 'POST', '/auth/login', ADMIN)).access_token
  await api(request, 'PUT', '/settings', { trial_days: '0', billing_grace_days: '0' })
  await api(request, 'POST', '/plans', { name: 'Browser Monthly', price: 1500, currency: 'KES', validity_days: 30, max_devices: 1 })
    .catch((e) => { if (!String(e).includes('409')) throw e }) // already there from an earlier run
})

test('staff sign in through the login page', async ({ page }) => {
  await page.goto('/login')
  await field(page, 'Username').fill(ADMIN.username)
  await field(page, 'Password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign In' }).click()
  await expect(page).not.toHaveURL(/login/)
})

test.describe('a customer, from sign-up to a reversed payment', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/login')
    await field(page, 'Username').fill(ADMIN.username)
    await field(page, 'Password').fill(ADMIN.password)
    await page.getByRole('button', { name: 'Sign In' }).click()
    await expect(page).not.toHaveURL(/login/)
  })

  test('creating a customer on a plan raises its invoice and grants nothing yet', async ({ page, request }) => {
    await page.goto('/users')
    await page.getByRole('button', { name: 'Add User' }).click()
    const modal = page.locator('.modal')
    await field(modal, 'Username').fill(CUSTOMER)
    await field(modal, 'Password').fill('cust-pass-123')
    await field(modal, 'Phone').fill('+254700000123')
    const plan = field(modal, 'Plan')
    const value = await plan.locator('option', { hasText: 'Browser Monthly' }).first().getAttribute('value')
    await plan.selectOption(value)
    await modal.getByRole('button', { name: 'Create' }).click()
    await expect(modal).toBeHidden()

    const u = await customer(request)
    expect(u, 'customer exists').toBeTruthy()
    expect(day(u.account_expiry), 'no paid time before paying').not.toBe(inDays(30))
    const inv = await api(request, 'GET', `/invoices?username=${CUSTOMER}`)
    const open = (inv.data || inv.invoices || []).filter((i) => ['pending', 'overdue'].includes(i.status))
    expect(open.length, 'one open invoice').toBe(1)
    expect(Number(open[0].amount)).toBe(1500)
  })

  test('paying the invoice on the Billing page adds the plan days', async ({ page, request }) => {
    await page.goto('/billing')
    const row = page.locator('tr', { hasText: CUSTOMER })
    await row.getByRole('button', { name: 'Record payment' }).click()
    const modal = page.locator('.modal')
    const amount = field(modal, 'Amount')
    if (!(await amount.inputValue())) await amount.fill('1500')
    await modal.getByRole('button', { name: 'Record Payment' }).click()
    await modal.getByRole('button', { name: 'Done' }).click()

    await expect.poll(async () => day((await customer(request)).account_expiry)).toBe(inDays(30))
  })

  test('editing a customer whose payment arrived after the page loaded keeps it (B31)', async ({ page, request }) => {
    await page.goto('/users')
    const row = page.locator('tr', { hasText: CUSTOMER })
    await expect(row).toBeVisible()

    // An M-Pesa renewal arrives while staff have the Users page open.
    const plans = await api(request, 'GET', '/plans')
    const plan = (plans.data || plans.plans || plans).find((p) => p.name === 'Browser Monthly')
    const inv = await api(request, 'POST', '/invoices', { username: CUSTOMER, plan_id: plan.id })
    const invID = inv.id || inv.invoice?.id || inv.data?.id
    await api(request, 'POST', '/payments', { invoice_id: invID, amount: 1500, payment_method: 'mobile_money' })
    expect(day((await customer(request)).account_expiry)).toBe(inDays(60))

    // Staff fix the phone number from the page loaded before the payment.
    await row.locator('button[title="Edit"]').click()
    const modal = page.locator('.modal')
    await field(modal, 'Phone').fill('+254700000999')
    await modal.getByRole('button', { name: 'Save Changes' }).click()
    await expect(modal).toBeHidden()

    const u = await customer(request)
    expect(u.phone).toBe('+254700000999')
    expect(day(u.account_expiry), 'the renewal is still there').toBe(inDays(60))
  })

  test('reversing the payment on the Payments page takes the days back', async ({ page, request }) => {
    await page.goto('/payments')
    page.once('dialog', (d) => d.accept('browser test: cheque bounced'))
    // The newest payment (the renewal) is listed first.
    await page.locator('tr', { hasText: CUSTOMER }).first().locator('button[title="Reverse payment"]').click()

    await expect.poll(async () => day((await customer(request)).account_expiry)).toBe(inDays(30))
    const inv = await api(request, 'GET', `/invoices?username=${CUSTOMER}`)
    const open = (inv.data || inv.invoices || []).filter((i) => ['pending', 'overdue'].includes(i.status))
    expect(open.length, 'the invoice is open again').toBe(1)
  })
})
