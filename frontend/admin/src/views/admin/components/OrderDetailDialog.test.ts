import { createApp, h, nextTick, reactive, type App } from 'vue'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type { AdminOrder } from '@/api/types'
import OrderDetailDialog from './OrderDetailDialog.vue'

const api = vi.hoisted(() => ({
  getOrder: vi.fn(),
  getProcurementOrders: vi.fn(),
  refundDirectOrderToWallet: vi.fn(),
  manualRefundDirectOrder: vi.fn(),
}))
const confirmAction = vi.hoisted(() => vi.fn())

vi.mock('@/api/admin', () => ({ adminAPI: api }))
vi.mock('@/stores/auth', () => ({ useAdminAuthStore: () => ({ hasPermission: () => true }) }))
vi.mock('@/utils/confirm', () => ({ confirmAction }))
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

const orderFixture = (id: number, status = 'fulfilling', stage = 'manual_delivery', active = false): AdminOrder => {
  const timestamp = '2026-11-18T08:00:00Z'
  return {
    id,
    order_no: `DIRECT-${id}`,
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
      child_id: id + 100,
      stage,
      updated_at: timestamp,
      reason: stage === 'manual_review' ? 'outcome_unknown' : undefined,
      attempt: { id: id + 200, sequence: 1, active },
    },
  }
}

const response = <T>(data: T) => ({ data: { data } })

const deferred = <T>() => {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((callback) => { resolve = callback })
  return { promise, resolve }
}

let host: HTMLDivElement
let app: App | undefined

beforeEach(() => {
  vi.resetAllMocks()
  api.getProcurementOrders.mockResolvedValue({ data: { data: [] } })
  confirmAction.mockResolvedValue(true)
  host = document.createElement('div')
  document.body.append(host)
})

afterEach(() => {
  app?.unmount()
  app = undefined
  host.remove()
})

const mountDialog = () => {
  const context = reactive({ open: false, order: null as AdminOrder | null })
  const onRefresh = vi.fn()
  app = createApp({
    render: () => h(OrderDetailDialog, {
      modelValue: context.open,
      order: context.order,
      siteCurrency: 'USD',
      maxRefundDays: 0,
      onRefresh,
      'onUpdate:modelValue': (open: boolean) => {
        context.open = open
        if (!open) context.order = null
      },
    }),
  })
  app.mount(host)
  return { context, onRefresh }
}

const findButton = (label: string) => Array.from(host.querySelectorAll('button'))
  .find((button) => button.textContent?.trim() === label)

const settle = async () => {
  await new Promise<void>((resolve) => setTimeout(resolve, 0))
  await nextTick()
}

it.each([
  { status: 'paid', stage: 'manual_delivery', active: false, canResolve: true, canReconcile: false },
  { status: 'fulfilling', stage: 'manual_delivery', active: false, canResolve: true, canReconcile: false },
  { status: 'fulfilling', stage: 'manual_delivery', active: true, canResolve: false, canReconcile: false },
  { status: 'fulfilling', stage: 'manual_review', active: true, canResolve: false, canReconcile: true },
  { status: 'refunded', stage: 'manual_delivery', active: false, canResolve: false, canReconcile: false },
])('shows eligible Direct actions for $status / $stage / active=$active', async ({ status, stage, active, canResolve, canReconcile }) => {
  const order = orderFixture(42, status, stage, active)
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
  expect(Boolean(findButton('admin.orders.directReview.complete'))).toBe(canResolve)
  expect(Boolean(findButton('admin.orders.directReview.release'))).toBe(canResolve)
  expect(Boolean(findButton('admin.orders.refundSubmit'))).toBe(canResolve)
  expect(Boolean(findButton('admin.orders.directReview.reconcile'))).toBe(canReconcile)
})

it('ignores an earlier order detail when switching orders while the dialog is open', async () => {
  const previous = orderFixture(42)
  const current = orderFixture(84, 'refunded')
  const pendingDetail = deferred<ReturnType<typeof response<AdminOrder>>>()
  api.getOrder.mockReturnValueOnce(pendingDetail.promise).mockResolvedValueOnce(response(current))
  const { context } = mountDialog()
  context.order = previous
  context.open = true
  await nextTick()
  expect(api.getOrder.mock.calls).toEqual([[previous.id]])

  context.order = current
  await vi.waitFor(() => expect(host.textContent).toContain(current.order_no))
  pendingDetail.resolve(response(previous))
  await settle()

  expect(host.textContent).toContain(current.order_no)
  expect(host.textContent).not.toContain(previous.order_no)
  expect(findButton('admin.orders.directReview.complete')).toBeUndefined()
  expect(api.getOrder.mock.calls).toEqual([[previous.id], [current.id]])
  expect(api.getProcurementOrders.mock.calls).toEqual([[{ order_no: current.order_no, page_size: 1 }]])
})

it('does not mix procurement details from an earlier order into the reopened dialog', async () => {
  const previous = orderFixture(42)
  const current = orderFixture(84)
  const previousProcurement = [{ upstream_order_no: 'PREVIOUS-PROCUREMENT' }]
  const currentProcurement = [{ upstream_order_no: 'CURRENT-PROCUREMENT' }]
  const pendingProcurement = deferred<ReturnType<typeof response<typeof previousProcurement>>>()
  api.getOrder.mockResolvedValueOnce(response(previous)).mockResolvedValueOnce(response(current))
  api.getProcurementOrders.mockReturnValueOnce(pendingProcurement.promise).mockResolvedValueOnce(response(currentProcurement))
  const { context } = mountDialog()
  context.order = previous
  context.open = true
  await vi.waitFor(() => expect(api.getProcurementOrders).toHaveBeenCalledWith({ order_no: previous.order_no, page_size: 1 }))

  context.open = false
  await nextTick()
  context.order = current
  context.open = true
  await vi.waitFor(() => expect(host.textContent).toContain('CURRENT-PROCUREMENT'))
  pendingProcurement.resolve(response(previousProcurement))
  await settle()

  expect(host.textContent).toContain(current.order_no)
  expect(host.textContent).toContain('CURRENT-PROCUREMENT')
  expect(host.textContent).not.toContain('PREVIOUS-PROCUREMENT')
})

it.each([
  { action: 'wallet refund on a different order', manual: false, currentId: 84 },
  { action: 'manual refund after reopening the same order', manual: true, currentId: 42 },
])('ignores late dialog updates from a $action while still refreshing the list', async ({ manual, currentId }) => {
  const previous = orderFixture(42)
  const current = orderFixture(currentId)
  const previousRefund = deferred<void>()
  const currentRefund = deferred<void>()
  const submitRefund = manual ? api.manualRefundDirectOrder : api.refundDirectOrderToWallet
  submitRefund.mockReturnValueOnce(previousRefund.promise).mockReturnValueOnce(currentRefund.promise)
  api.getOrder.mockResolvedValueOnce(response(previous)).mockResolvedValue(response(current))
  const { context, onRefresh } = mountDialog()

  const enterAndSubmit = async (note: string) => {
    if (manual) {
      findButton('admin.orders.refundTabManual')!.click()
      await nextTick()
    }
    const placeholder = manual ? 'manualRefundReasonPlaceholder' : 'refundRemarkPlaceholder'
    const field = host.querySelector<HTMLInputElement>(`input[placeholder="admin.orders.${placeholder}"]`)!
    field.value = note
    field.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()
    field.form!.dispatchEvent(new Event('submit', { cancelable: true }))
    await settle()
    return field
  }

  context.order = previous
  context.open = true
  await vi.waitFor(() => expect(host.textContent).toContain(previous.order_no))
  await enterAndSubmit('previous refund')
  expect(submitRefund).toHaveBeenNthCalledWith(1, previous.id, expect.objectContaining({ remark: 'previous refund' }))

  context.open = false
  await nextTick()
  context.order = current
  context.open = true
  await vi.waitFor(() => expect(host.textContent).toContain(current.order_no))
  const currentField = await enterAndSubmit('current refund')
  expect(submitRefund).toHaveBeenNthCalledWith(2, current.id, expect.objectContaining({ remark: 'current refund' }))

  previousRefund.resolve()
  await settle()
  expect(onRefresh).toHaveBeenCalledTimes(1)
  expect(api.getOrder.mock.calls).toEqual([[previous.id], [current.id]])
  expect(currentField.value).toBe('current refund')
  expect(currentField.disabled).toBe(true)
  expect(host.textContent).not.toContain(manual ? 'admin.orders.manualRefundSuccess' : 'admin.orders.refundSuccess')

  currentRefund.resolve()
  await vi.waitFor(() => expect(onRefresh).toHaveBeenCalledTimes(2))
  expect(api.getOrder.mock.calls).toEqual([[previous.id], [current.id], [current.id]])
})
