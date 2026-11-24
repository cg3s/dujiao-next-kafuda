import { createApp, h, nextTick, reactive, type App } from 'vue'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type { AdminOrder } from '@/api/types'
import OrderDetailDialog from './OrderDetailDialog.vue'

const api = vi.hoisted(() => ({
  getOrder: vi.fn(),
  getProcurementOrders: vi.fn(),
  reconcileDirectOrder: vi.fn(),
  confirmDirectUpstreamCompletion: vi.fn(),
  refundDirectOrderToWallet: vi.fn(),
  manualRefundDirectOrder: vi.fn(),
}))
const confirmAction = vi.hoisted(() => vi.fn())
const hasPermission = vi.hoisted(() => vi.fn())

vi.mock('@/api/admin', () => ({ adminAPI: api }))
vi.mock('@/stores/auth', () => ({ useAdminAuthStore: () => ({ hasPermission }) }))
vi.mock('@/utils/confirm', () => ({ confirmAction }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  const { ref } = await import('vue')
  return { ...actual, useI18n: () => ({ t: (key: string) => key, te: () => true, locale: ref('en-US') }) }
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
    refund_window_expired: false,
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

const upstreamCompletionFixture = (id: number, status = 'fulfilling', active = false): AdminOrder => {
  const order = orderFixture(id, status, 'manual_review', active)
  const recharge = order.direct_recharge!
  recharge.reason = 'cost_limit_exceeded'
  recharge.can_confirm_upstream_completion = true
  recharge.cost_limit_usd = '12.50'
  recharge.attempt!.upstream_order_id = '9007199254740993'
  recharge.attempt!.card_charged_usd = '15.46'
  return order
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
  hasPermission.mockReturnValue(true)
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

const reconcileInput = () => host.querySelector<HTMLInputElement>('#direct-reconcile-id')

const enterReconciliationID = async (id: string) => {
  const input = reconcileInput()
  expect(input).not.toBeNull()
  input!.value = id
  input!.dispatchEvent(new Event('input', { bubbles: true }))
  await nextTick()
  return input!
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

it.each(['completed', 'known upstream ID'])('loads authoritative %s after a rejected Direct reconciliation', async (result) => {
  const order = orderFixture(42, 'fulfilling', 'manual_review', true)
  const completed = result === 'completed'
  const latest = orderFixture(42, completed ? 'completed' : 'fulfilling', completed ? 'completed' : 'manual_review', !completed)
  const upstreamID = '9007199254740993'
  latest.direct_recharge!.attempt!.upstream_order_id = upstreamID
  api.getOrder.mockResolvedValueOnce(response(order)).mockResolvedValueOnce(response(latest))
  api.reconcileDirectOrder.mockRejectedValueOnce(new Error(completed ? 'Response lost' : 'Order status invalid'))
  const { context, onRefresh } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(reconcileInput()).not.toBeNull())
  const input = await enterReconciliationID(upstreamID)
  input.form!.dispatchEvent(new Event('submit', { cancelable: true }))
  await vi.waitFor(() => expect(host.textContent).toContain(upstreamID))

  expect(host.textContent).toContain(`admin.orders.directReview.stages.${latest.direct_recharge!.stage}`)
  expect(reconcileInput()).toBeNull()
  expect(api.getOrder.mock.calls).toEqual([[order.id], [order.id]])
  expect(api.reconcileDirectOrder.mock.calls).toEqual([[order.direct_recharge!.child_id, order.direct_recharge!.attempt!.id, upstreamID]])
  expect(onRefresh).toHaveBeenCalledOnce()
})

it.each([true, false])('preserves the typed ID when the attempt remains eligible (refresh succeeds=%s)', async (readSucceeded) => {
  const order = orderFixture(42, 'fulfilling', 'manual_review', true)
  const pending = deferred<ReturnType<typeof response<AdminOrder>>>()
  api.getOrder.mockResolvedValueOnce(response(order))
  if (readSucceeded) api.getOrder.mockReturnValueOnce(pending.promise)
  else api.getOrder.mockRejectedValueOnce(new Error('Refresh failed'))
  api.reconcileDirectOrder.mockRejectedValueOnce(new Error('Lookup failed'))
  const { context, onRefresh } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(reconcileInput()).not.toBeNull())
  const input = await enterReconciliationID('123456789')
  input.form!.dispatchEvent(new Event('submit', { cancelable: true }))
  if (readSucceeded) {
    await vi.waitFor(() => expect(reconcileInput()?.disabled).toBe(true))
    pending.resolve(response(orderFixture(42, 'fulfilling', 'manual_review', true)))
  }
  await vi.waitFor(() => expect(reconcileInput()?.disabled).toBe(false))

  expect(reconcileInput()?.value).toBe('123456789')
  expect(api.getOrder.mock.calls).toEqual([[order.id], [order.id]])
  expect(api.reconcileDirectOrder).toHaveBeenCalledOnce()
  expect(onRefresh).not.toHaveBeenCalled()
})

it('discards the reconciliation refresh if the operator switches orders', async () => {
  const previous = orderFixture(42, 'fulfilling', 'manual_review', true)
  const current = orderFixture(84, 'fulfilling', 'manual_review', true)
  const pending = deferred<ReturnType<typeof response<AdminOrder>>>()
  api.getOrder.mockResolvedValueOnce(response(previous)).mockReturnValueOnce(pending.promise).mockResolvedValueOnce(response(current))
  api.reconcileDirectOrder.mockRejectedValueOnce(new Error('Response lost'))
  const { context, onRefresh } = mountDialog()
  context.order = previous
  context.open = true

  await vi.waitFor(() => expect(reconcileInput()).not.toBeNull())
  const original = await enterReconciliationID('9007199254740993')
  original.form!.dispatchEvent(new Event('submit', { cancelable: true }))
  await vi.waitFor(() => expect(api.getOrder.mock.calls).toEqual([[previous.id], [previous.id]]))
  context.order = current
  await vi.waitFor(() => expect(host.textContent).toContain(current.order_no))
  await enterReconciliationID('4321')
  pending.resolve(response(orderFixture(previous.id, 'completed', 'completed')))
  await settle()

  expect(host.textContent).toContain(current.order_no)
  expect(host.textContent).not.toContain(previous.order_no)
  expect(reconcileInput()?.value).toBe('4321')
  expect(api.getOrder.mock.calls).toEqual([[previous.id], [previous.id], [current.id]])
  expect(api.reconcileDirectOrder).toHaveBeenCalledOnce()
  expect(onRefresh).not.toHaveBeenCalled()
})

it.each([
  { direct: false, expired: false, canRefund: true },
  { direct: false, expired: true, canRefund: false },
  { direct: false, expired: undefined, canRefund: false },
  { direct: true, expired: true, canRefund: false },
  { direct: true, expired: undefined, canRefund: false },
])('uses the server refund window for direct=$direct / expired=$expired', async ({ direct, expired, canRefund }) => {
  const order = orderFixture(42)
  if (!direct) delete order.direct_recharge
  order.refund_window_expired = expired
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
  expect(Boolean(findButton('admin.orders.refundSubmit'))).toBe(canRefund)
  expect(Boolean(findButton('admin.orders.refundTabManual'))).toBe(canRefund)
  expect(Boolean(findButton('admin.orders.directReview.complete'))).toBe(direct)
})

it.each([
  { direct: false, action: 'refund-to-wallet' },
  { direct: false, action: 'manual-refund' },
  { direct: true, action: 'refund-to-wallet' },
  { direct: true, action: 'manual-refund' },
])('shows only the granted refund action and skips procurement for direct=$direct / $action', async ({ direct, action }) => {
  const order = orderFixture(42)
  if (!direct) delete order.direct_recharge
  const endpoint = direct ? `direct-${action}` : action
  hasPermission.mockImplementation((permission: string) => permission === `POST:/admin/orders/${order.id}/${endpoint}`)
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
  expect(hasPermission).toHaveBeenCalledWith('GET:/admin/procurement-orders')
  expect(api.getProcurementOrders).not.toHaveBeenCalled()
  const wallet = action === 'refund-to-wallet'
  expect(Boolean(findButton('admin.orders.refundTabWallet'))).toBe(wallet)
  expect(Boolean(findButton('admin.orders.refundTabManual'))).toBe(!wallet)
  expect(Boolean(findButton('admin.orders.refundSubmit'))).toBe(wallet)
  expect(Boolean(findButton('admin.orders.manualRefundSubmit'))).toBe(!wallet)
})

it.each([undefined, '0.00', '15.46'])('shows a reported card charge in USD (%s)', async (cardCharge) => {
  const order = orderFixture(42, 'completed', 'completed')
  order.currency = 'CNY'
  order.direct_recharge!.attempt!.card_charged_usd = cardCharge
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
  const chargeLabel = Array.from(host.querySelectorAll('dt')).find((label) => label.textContent === 'admin.orders.directReview.cardCharged')
  expect(chargeLabel?.nextElementSibling?.textContent).toBe(cardCharge === undefined ? undefined : `${cardCharge} USD`)
})

it('confirms upstream activation once and refreshes the completed order', async () => {
  const order = upstreamCompletionFixture(42)
  order.currency = 'CNY'
  const completed = orderFixture(order.id, 'completed', 'completed')
  const confirmation = deferred<boolean>()
  const request = deferred<void>()
  confirmAction.mockReturnValueOnce(confirmation.promise)
  api.confirmDirectUpstreamCompletion.mockReturnValueOnce(request.promise)
  api.getOrder.mockResolvedValueOnce(response(order)).mockResolvedValueOnce(response(completed))
  const { context, onRefresh } = mountDialog()
  context.order = order
  context.open = true

  const action = 'admin.orders.directReview.confirmUpstream'
  await vi.waitFor(() => expect(findButton(action)).toBeDefined())
  const amountLabels = Array.from(host.querySelectorAll('dt'))
  expect(amountLabels.find((node) => node.textContent === 'admin.orders.directReview.cardCharged')?.nextElementSibling?.textContent).toBe('15.46 USD')
  expect(amountLabels.find((node) => node.textContent === 'admin.orders.directReview.costLimit')?.nextElementSibling?.textContent).toBe('12.50 USD')
  expect(findButton('admin.orders.directReview.complete')).toBeUndefined()
  expect(findButton('admin.orders.directReview.release')).toBeUndefined()
  expect(findButton('admin.orders.refundSubmit')).toBeUndefined()
  findButton(action)!.click()
  findButton(action)!.click()
  expect(confirmAction).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ title: action, description: `${action}Prompt` }))
  expect(api.confirmDirectUpstreamCompletion).not.toHaveBeenCalled()

  confirmation.resolve(true)
  await vi.waitFor(() => expect(api.confirmDirectUpstreamCompletion).toHaveBeenCalledExactlyOnceWith(order.direct_recharge!.child_id, order.direct_recharge!.attempt!.id))
  expect(findButton(action)?.disabled).toBe(true)
  request.resolve()
  await vi.waitFor(() => expect(onRefresh).toHaveBeenCalledTimes(1))
  expect(api.getOrder.mock.calls).toEqual([[order.id], [order.id]])
  expect(findButton(action)).toBeUndefined()
})

it.each([
  { situation: 'the backend did not flag the order', eligible: undefined },
  { situation: 'the operator lacks permission', authorized: false, eligible: true },
  { situation: 'the shop order is complete', status: 'completed', eligible: true },
  { situation: 'the current attempt is still active', active: true, eligible: true },
  { situation: 'the supplier ID is missing', missingUpstreamID: true, eligible: true },
])('omits the upstream confirmation action when $situation', async ({ authorized = true, status, active, eligible, missingUpstreamID }) => {
  const order = upstreamCompletionFixture(42, status, active)
  const recharge = order.direct_recharge!
  recharge.can_confirm_upstream_completion = eligible
  if (missingUpstreamID) delete recharge.attempt!.upstream_order_id
  hasPermission.mockImplementation((permission: string) => authorized || permission !== 'POST:/admin/orders/:id/direct-confirm-upstream')
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
  expect(findButton('admin.orders.directReview.confirmUpstream')).toBeUndefined()
})

it('discards an upstream confirmation after reopening the same order', async () => {
  const order = upstreamCompletionFixture(42)
  const confirmation = deferred<boolean>()
  confirmAction.mockReturnValueOnce(confirmation.promise)
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  const action = 'admin.orders.directReview.confirmUpstream'
  context.order = order
  context.open = true
  await vi.waitFor(() => expect(findButton(action)).toBeDefined())
  findButton(action)!.click()

  context.open = false
  await nextTick()
  context.open = true
  await vi.waitFor(() => expect(findButton(action)?.disabled).toBe(false))
  confirmation.resolve(true)
  await settle()

  expect(api.confirmDirectUpstreamCompletion).not.toHaveBeenCalled()
  expect(findButton(action)?.disabled).toBe(false)
})

it.each([
  { situation: 'legacy order', quoted: false, cardCharge: '15.46' },
  { situation: 'submission before upstream acknowledgement', quoted: true, cardCharge: undefined },
  { situation: 'completed order with an actual card charge', quoted: true, cardCharge: '15.46' },
])('shows the USD quote breakdown only when present for $situation', async ({ quoted, cardCharge }) => {
  const order = cardCharge ? orderFixture(42, 'completed', 'completed') : orderFixture(42, 'paid', 'submitting', true)
  order.currency = 'CNY'
  const attempt = order.direct_recharge!.attempt!
  attempt.card_charged_usd = cardCharge
  if (cardCharge) attempt.upstream_order_id = '1234'
  if (quoted) {
    attempt.quote_costs = {
      service_fee_usd: '0.00',
      card_open_fee_usd: '1.25',
      card_load_usd: '15.00',
      total_usd: '16.25',
    }
  }
  api.getOrder.mockResolvedValue(response(order))
  const { context } = mountDialog()
  context.order = order
  context.open = true

  await vi.waitFor(() => expect(host.textContent).toContain(order.order_no))
  const quoteTitle = Array.from(host.querySelectorAll('h4')).find((node) => node.textContent === 'admin.orders.directReview.quoteCosts.title')
  expect(Boolean(quoteTitle)).toBe(quoted)
  const quoteLabels = Array.from(quoteTitle?.nextElementSibling?.querySelectorAll('dt') ?? [])
  expect(quoteLabels.map((label) => [label.textContent, label.nextElementSibling?.textContent])).toEqual(quoted ? [
    ['admin.orders.directReview.quoteCosts.serviceFee', '0.00 USD'],
    ['admin.orders.directReview.quoteCosts.cardOpenFee', '1.25 USD'],
    ['admin.orders.directReview.quoteCosts.cardLoad', '15.00 USD'],
    ['admin.orders.directReview.quoteCosts.total', '16.25 USD'],
  ] : [])
  const cardLabel = Array.from(host.querySelectorAll('dt')).find((node) => node.textContent === 'admin.orders.directReview.cardCharged')
  expect(cardLabel?.nextElementSibling?.textContent).toBe(cardCharge ? `${cardCharge} USD` : undefined)
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
