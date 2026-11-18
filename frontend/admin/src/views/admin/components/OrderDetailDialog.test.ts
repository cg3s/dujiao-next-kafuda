import { createApp, h, ref } from 'vue'
import { expect, it, vi } from 'vitest'
import type { AdminOrder } from '@/api/types'
import OrderDetailDialog from './OrderDetailDialog.vue'

const api = vi.hoisted(() => ({ getOrder: vi.fn(), getProcurementOrders: vi.fn() }))

vi.mock('@/api/admin', () => ({ adminAPI: api }))
vi.mock('@/stores/auth', () => ({ useAdminAuthStore: () => ({ hasPermission: () => true }) }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  const { ref } = await import('vue')
  return { ...actual, useI18n: () => ({ t: (key: string) => key, locale: ref('en-US') }) }
})
vi.mock('@/components/ui/dialog', async () => {
  const { defineComponent } = await import('vue')
  const passthrough = defineComponent({
    inheritAttrs: false,
    setup: (_, { slots }) => () => slots.default?.(),
  })
  return {
    Dialog: defineComponent({
      props: { open: Boolean },
      setup: (props, { slots }) => () => props.open ? slots.default?.() : null,
    }),
    DialogScrollContent: passthrough,
    DialogHeader: passthrough,
    DialogTitle: passthrough,
  }
})

it.each([
  { status: 'paid', stage: 'manual_delivery', active: false, canResolve: true, canReconcile: false },
  { status: 'fulfilling', stage: 'manual_delivery', active: false, canResolve: true, canReconcile: false },
  { status: 'fulfilling', stage: 'manual_delivery', active: true, canResolve: false, canReconcile: false },
  { status: 'fulfilling', stage: 'manual_review', active: true, canResolve: false, canReconcile: true },
  { status: 'refunded', stage: 'manual_delivery', active: false, canResolve: false, canReconcile: false },
])('shows eligible Direct actions for $status / $stage / active=$active', async ({ status, stage, active, canResolve, canReconcile }) => {
  const timestamp = '2026-11-18T08:00:00Z'
  const order: AdminOrder = {
    id: 42,
    order_no: 'DIRECT-42',
    user_id: 1,
    status,
    currency: 'USD',
    original_amount: 10,
    discount_amount: 0,
    promotion_discount_amount: 0,
    total_amount: 10,
    wallet_paid_amount: 10,
    online_paid_amount: 0,
    refunded_amount: status === 'refunded' ? 10 : 0,
    created_at: timestamp,
    paid_at: timestamp,
    updated_at: timestamp,
    direct_recharge: {
      child_id: 43,
      stage,
      updated_at: timestamp,
      reason: stage === 'manual_review' ? 'outcome_unknown' : undefined,
      attempt: { id: 55, sequence: 1, active },
    },
  }
  api.getOrder.mockResolvedValue({ data: { data: order } })
  api.getProcurementOrders.mockResolvedValue({ data: { data: [] } })
  const host = document.createElement('div')
  document.body.append(host)
  const open = ref(false)
  const app = createApp({ render: () => h(OrderDetailDialog, { modelValue: open.value, order, siteCurrency: 'USD', maxRefundDays: 0 }) })
  app.mount(host)

  try {
    open.value = true
    await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
    const buttons = Array.from(host.querySelectorAll('button'), button => button.textContent?.trim())
    expect(buttons.includes('admin.orders.directReview.complete')).toBe(canResolve)
    expect(buttons.includes('admin.orders.directReview.release')).toBe(canResolve)
    expect(buttons.includes('admin.orders.refundSubmit')).toBe(canResolve)
    expect(buttons.includes('admin.orders.directReview.reconcile')).toBe(canReconcile)
  } finally {
    app.unmount()
    host.remove()
  }
})
