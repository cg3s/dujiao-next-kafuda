import { KAFUDA_GPT_DIRECT } from './directPurchase.ts'

const DIRECT_ORDER_STAGES = ['waiting_session', 'checking', 'submitting', 'processing', 'manual_review', 'completed', 'refunded'] as const

export type DirectOrderStage = typeof DIRECT_ORDER_STAGES[number]
type TerminalDirectOrderStage = Extract<DirectOrderStage, 'completed' | 'refunded'>

export interface DirectOrderStatus {
  stage: DirectOrderStage
  updated_at: string
  renewal_cancelled?: boolean
}

type DirectOrderCandidate = {
  order_no?: string
  status?: string
  items?: { fulfillment_type?: string }[]
  children?: { items?: { fulfillment_type?: string }[] }[]
}

export function directStatusOrderNo(order: DirectOrderCandidate | null): string {
  if (!order?.order_no || (order.status !== 'paid' && order.status !== 'completed')) return ''
  const child = order.children?.length === 1 ? order.children[0] : undefined
  return child?.items?.length === 1 && child.items[0]?.fulfillment_type === KAFUDA_GPT_DIRECT ? order.order_no : ''
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

export function startDirectOrderPolling(
  fetchStatus: () => Promise<unknown>,
  onUpdate: (status: DirectOrderStatus | null) => void,
  page: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>,
  syncTerminalOrder?: (stage: TerminalDirectOrderStage) => Promise<boolean>,
): () => void {
  let stopped = false
  let inFlight = false
  let lastStage: DirectOrderStage | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  const stop = () => {
    stopped = true
    clearTimeout(timer)
    page.removeEventListener('visibilitychange', onVisibilityChange)
  }
  const refresh = async () => {
    if (stopped || inFlight || page.visibilityState === 'hidden') return
    inFlight = true
    if ((lastStage === 'completed' || lastStage === 'refunded') && syncTerminalOrder) {
      try {
        if (await syncTerminalOrder(lastStage)) stop()
      } catch {
        // Keep the confirmed terminal stage while the order detail catches up.
      } finally {
        inFlight = false
      }
      if (!stopped && page.visibilityState === 'visible') {
        timer = setTimeout(refresh, ACTIVE_POLL_INTERVAL)
      }
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
    if (lastStage === 'completed' || lastStage === 'refunded') {
      if (!syncTerminalOrder) {
        stop()
        return
      }
      void refresh()
    } else if (!stopped && page.visibilityState === 'visible') {
      const interval = lastStage === 'manual_review' ? MANUAL_REVIEW_POLL_INTERVAL : ACTIVE_POLL_INTERVAL
      timer = setTimeout(refresh, interval)
    }
  }
  const onVisibilityChange = () => {
    clearTimeout(timer)
    void refresh()
  }
  page.addEventListener('visibilitychange', onVisibilityChange)
  void refresh()
  return stop
}
