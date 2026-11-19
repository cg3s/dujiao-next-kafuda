import { isDirectFulfillmentType } from './directPurchase.ts'

const DIRECT_ORDER_STAGES = ['waiting_session', 'checking', 'submitting', 'processing', 'manual_review', 'completed', 'refunded'] as const

export type DirectOrderStage = typeof DIRECT_ORDER_STAGES[number]
type TerminalDirectOrderStage = Extract<DirectOrderStage, 'completed' | 'refunded'>

export interface DirectOrderStatus {
  stage: DirectOrderStage
  updated_at: string
  renewal_cancelled?: boolean
}

type OrderItemFulfillment = { fulfillment_type?: string }

type DirectOrderCandidate = {
  order_no?: string
  status?: string
  items?: OrderItemFulfillment[]
  children?: { items?: OrderItemFulfillment[] }[]
}

export function isDirectOrderFulfillment(items?: readonly OrderItemFulfillment[], fulfillmentType?: string): boolean {
  return isDirectFulfillmentType(fulfillmentType) || (items?.length === 1 && isDirectFulfillmentType(items[0]?.fulfillment_type))
}

function directParentOrderNo(order: DirectOrderCandidate): string {
  const child = order.children?.length === 1 ? order.children[0] : undefined
  return isDirectOrderFulfillment(child?.items) ? order.order_no || '' : ''
}

export function directPendingPaymentOrderNo(order: DirectOrderCandidate | null): string {
  return order?.status === 'pending_payment' ? directParentOrderNo(order) : ''
}

export function directStatusOrderNo(order: DirectOrderCandidate | null): string {
  if (!order || !['paid', 'fulfilling', 'completed', 'refunded'].includes(order.status ?? '')) return ''
  return directParentOrderNo(order)
}

export function parseDirectOrderStatus(input: unknown): DirectOrderStatus | null {
  if (!input || typeof input !== 'object') return null
  const { stage: rawStage, updated_at, renewal_cancelled } = input as Partial<DirectOrderStatus>
  const stage = DIRECT_ORDER_STAGES.find((candidate) => candidate === rawStage)
  if (!stage) return null
  if (typeof updated_at !== 'string' || !Number.isFinite(Date.parse(updated_at))) return null
  if (stage === 'completed' && typeof renewal_cancelled === 'boolean') {
    return { stage, updated_at, renewal_cancelled }
  }
  return { stage, updated_at }
}

const ACTIVE_POLL_INTERVAL = 10_000
const MANUAL_REVIEW_POLL_INTERVAL = 60_000

type DirectOrderConnectivity = {
  isOnline: () => boolean
  events: Pick<EventTarget, 'addEventListener' | 'removeEventListener'>
}

export function startDirectOrderPolling(
  fetchStatus: () => Promise<unknown>,
  onUpdate: (status: DirectOrderStatus | null) => void,
  page: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>,
  syncTerminalOrder?: (stage: TerminalDirectOrderStage) => Promise<boolean>,
  connectivity?: DirectOrderConnectivity,
): () => void {
  let stopped = false
  let inFlight = false
  let reconnectedWhileInFlight = false
  let lastStage: DirectOrderStage | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  const isOnline = () => connectivity?.isOnline() ?? true
  const terminalStage = () => lastStage === 'completed' || lastStage === 'refunded' ? lastStage : undefined
  const stop = () => {
    stopped = true
    clearTimeout(timer)
    page.removeEventListener('visibilitychange', onVisibilityChange)
    connectivity?.events.removeEventListener('online', onOnline)
    connectivity?.events.removeEventListener('offline', onOffline)
    connectivity?.events.removeEventListener('pageshow', onPageShow)
  }
  const schedule = (interval: number, retryAfterReconnect: boolean) => {
    if (stopped || page.visibilityState !== 'visible' || !isOnline()) return
    if (retryAfterReconnect && reconnectedWhileInFlight) void refresh()
    else timer = setTimeout(refresh, interval)
  }
  const refresh = async () => {
    if (stopped || inFlight || page.visibilityState === 'hidden') return
    if (!isOnline()) {
      if (!terminalStage()) onUpdate(null)
      return
    }
    inFlight = true
    reconnectedWhileInFlight = false
    const confirmedStage = terminalStage()
    if (confirmedStage && syncTerminalOrder) {
      try {
        if (await syncTerminalOrder(confirmedStage)) stop()
      } catch {
        // Keep the confirmed terminal stage while the order detail catches up.
      } finally {
        inFlight = false
      }
      schedule(ACTIVE_POLL_INTERVAL, true)
      return
    }
    let status: DirectOrderStatus | null = null
    try {
      status = parseDirectOrderStatus(await fetchStatus())
    } catch {
      // A transport failure leaves the last confirmed stage available.
    } finally {
      inFlight = false
    }
    if (stopped) return
    if (status) lastStage = status.stage
    onUpdate(status)
    if (terminalStage()) {
      if (!syncTerminalOrder) {
        stop()
        return
      }
      void refresh()
    } else {
      if (status && !isOnline()) onUpdate(null)
      const interval = lastStage === 'manual_review' ? MANUAL_REVIEW_POLL_INTERVAL : ACTIVE_POLL_INTERVAL
      schedule(interval, status === null)
    }
  }
  const onVisibilityChange = () => {
    clearTimeout(timer)
    void refresh()
  }
  const onPageShow = (event: Event) => {
    if ((event as PageTransitionEvent).persisted) onVisibilityChange()
  }
  const onOffline = () => {
    clearTimeout(timer)
    if (!terminalStage()) onUpdate(null)
  }
  const onOnline = () => {
    clearTimeout(timer)
    if (page.visibilityState !== 'visible') return
    if (inFlight) reconnectedWhileInFlight = true
    else void refresh()
  }
  page.addEventListener('visibilitychange', onVisibilityChange)
  connectivity?.events.addEventListener('online', onOnline)
  connectivity?.events.addEventListener('offline', onOffline)
  connectivity?.events.addEventListener('pageshow', onPageShow)
  void refresh()
  return stop
}
