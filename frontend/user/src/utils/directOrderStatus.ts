import { KAFUDA_GPT_DIRECT } from './directPurchase.ts'

export type DirectOrderStage = 'waiting_session' | 'processing' | 'manual_review' | 'completed'

export interface DirectOrderStatus {
  stage: DirectOrderStage
  updated_at: string
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
  const { stage, updated_at } = input as Partial<DirectOrderStatus>
  if (stage !== 'waiting_session' && stage !== 'processing' && stage !== 'manual_review' && stage !== 'completed') return null
  if (typeof updated_at !== 'string' || !Number.isFinite(Date.parse(updated_at))) return null
  return { stage, updated_at }
}

export function startDirectOrderPolling(
  fetchStatus: () => Promise<unknown>,
  onUpdate: (status: DirectOrderStatus | null) => void,
): () => void {
  let stopped = false
  let timer: ReturnType<typeof setTimeout> | undefined
  const refresh = async () => {
    let status: DirectOrderStatus | null = null
    try {
      status = parseDirectOrderStatus(await fetchStatus())
    } catch {
      // A transport failure is an unavailable refresh, not an order outcome.
    }
    if (stopped) return
    onUpdate(status)
    if (!stopped && status?.stage !== 'completed') timer = setTimeout(refresh, 10_000)
  }
  void refresh()
  return () => {
    stopped = true
    clearTimeout(timer)
  }
}
