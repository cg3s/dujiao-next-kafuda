import { isHostedRechargeType } from './directPurchase.ts'

export const HOSTED_RECHARGE_STAGES = [
  'waiting_payment',
  'waiting_procurement',
  'procurement_blocked',
  'waiting_submit',
  'recharging',
  'resubmit_required',
  'manual_review',
  'refunding',
  'succeeded',
  'closed',
  'refunded',
] as const

export type HostedRechargeStage = typeof HOSTED_RECHARGE_STAGES[number]
export type TerminalHostedRechargeStage = Extract<HostedRechargeStage, 'succeeded' | 'closed' | 'refunded'>

export interface HostedRechargeStatus {
  stage: HostedRechargeStage
  progress_stage?: string
  progress_message?: string
  progress_updated_at?: string
  can_open_fulfillment: boolean
  updated_at: string
  completed_at?: string
}

type OrderItemFulfillment = { fulfillment_type?: string }

type HostedRechargeOrderCandidate = {
  order_no?: string
  status?: string
  items?: OrderItemFulfillment[]
  children?: { items?: OrderItemFulfillment[] }[]
}

export function isHostedRechargeOrderFulfillment(
  items?: readonly OrderItemFulfillment[],
  fulfillmentType?: string,
): boolean {
  return isHostedRechargeType(fulfillmentType)
    || Boolean(items?.some((item) => isHostedRechargeType(item?.fulfillment_type)))
}

export function isHostedRechargeOrder(order: HostedRechargeOrderCandidate | null | undefined): boolean {
  if (!order) return false
  if (isHostedRechargeOrderFulfillment(order.items)) return true
  return Boolean(order.children?.some((child) => isHostedRechargeOrderFulfillment(child?.items)))
}

function hostedRechargeParentOrderNo(order: HostedRechargeOrderCandidate): string {
  const child = order.children?.length === 1 ? order.children[0] : undefined
  if (isHostedRechargeOrderFulfillment(child?.items) || isHostedRechargeOrderFulfillment(order.items)) {
    return order.order_no || ''
  }
  return ''
}

export function hostedRechargeStatusOrderNo(order: HostedRechargeOrderCandidate | null): string {
  if (!order || !['paid', 'fulfilling', 'completed', 'refunded', 'canceled', 'expired'].includes(order.status ?? '')) {
    return ''
  }
  return hostedRechargeParentOrderNo(order)
}

export function isTerminalHostedRechargeStage(stage?: string): stage is TerminalHostedRechargeStage {
  return stage === 'succeeded' || stage === 'closed' || stage === 'refunded'
}

export function parseHostedRechargeStatus(input: unknown): HostedRechargeStatus | null {
  if (!input || typeof input !== 'object') return null
  const raw = input as Partial<HostedRechargeStatus>
  const stage = HOSTED_RECHARGE_STAGES.find((candidate) => candidate === raw.stage)
  if (!stage) return null
  if (typeof raw.updated_at !== 'string' || !Number.isFinite(Date.parse(raw.updated_at))) return null
  return {
    stage,
    progress_stage: typeof raw.progress_stage === 'string' && raw.progress_stage ? raw.progress_stage : undefined,
    progress_message: typeof raw.progress_message === 'string' && raw.progress_message ? raw.progress_message : undefined,
    progress_updated_at: typeof raw.progress_updated_at === 'string' && Number.isFinite(Date.parse(raw.progress_updated_at))
      ? raw.progress_updated_at
      : undefined,
    can_open_fulfillment: Boolean(raw.can_open_fulfillment),
    updated_at: raw.updated_at,
    completed_at: typeof raw.completed_at === 'string' && Number.isFinite(Date.parse(raw.completed_at))
      ? raw.completed_at
      : undefined,
  }
}

const ACTIVE_POLL_INTERVAL = 8_000
const SLOW_POLL_INTERVAL = 30_000

type HostedRechargeConnectivity = {
  isOnline: () => boolean
  events: Pick<EventTarget, 'addEventListener' | 'removeEventListener'>
}

export function startHostedRechargePolling(
  fetchStatus: () => Promise<unknown>,
  onUpdate: (status: HostedRechargeStatus | null) => void,
  page: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>,
  syncTerminalOrder?: (stage: TerminalHostedRechargeStage) => Promise<boolean>,
  connectivity?: HostedRechargeConnectivity,
): () => void {
  let stopped = false
  let inFlight = false
  let reconnectedWhileInFlight = false
  let lastStage: HostedRechargeStage | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  const isOnline = () => connectivity?.isOnline() ?? true
  const terminalStage = () => (isTerminalHostedRechargeStage(lastStage) ? lastStage : undefined)

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
        // Keep last confirmed stage while order detail catches up.
      } finally {
        inFlight = false
      }
      schedule(ACTIVE_POLL_INTERVAL, true)
      return
    }
    let status: HostedRechargeStatus | null = null
    try {
      status = parseHostedRechargeStatus(await fetchStatus())
    } catch {
      // Keep last confirmed stage on transient network errors.
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
      const slow = lastStage === 'manual_review' || lastStage === 'procurement_blocked'
      schedule(slow ? SLOW_POLL_INTERVAL : ACTIVE_POLL_INTERVAL, status === null)
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
