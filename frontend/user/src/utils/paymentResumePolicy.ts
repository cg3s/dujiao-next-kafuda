export type PaymentResetReason = 'generic' | 'route_change' | 'change_payment_method'

export interface PaymentResetPolicy {
  resumeLatestPayment: boolean
  clearSelectedChannel: boolean
  stopActivePaymentWatch: boolean
}

export interface CachedPaymentRestorePolicy {
  startActivePaymentWatch: boolean
  autoOpenPayLink: boolean
}

export type PaymentPresentationMode = 'qr' | 'redirect'
export type PaymentLinkNavigationTarget = 'current-tab' | 'new-window'
export type PaymentInteractionLabelKey =
  | 'payment.modeQr'
  | 'payment.modeRedirect'
  | 'payment.modeWap'
  | 'payment.modePage'
export type PaymentResultTitleKey =
  | 'payment.resultTitle'
  | 'payment.resultRedirectTitle'
  | 'payment.modeWap'
  | 'payment.modePage'

const REDIRECT_PAYMENT_INTERACTION_MODES = new Set(['redirect', 'wap', 'page'])
const normalizeInteractionMode = (mode: unknown) => String(mode || '').trim().toLowerCase()

export const PAYMENT_RETURN_MARKERS = ['epay_return', 'alipay_return', 'wechat_return', 'epusdt_return', 'bepusdt_return', 'tokenpay_return', 'okpay_return', 'pp_return', 'stripe_return']

type PaymentQuery = Record<string, unknown> | URLSearchParams

const paymentQueryValueToString = (value: unknown): string => {
  if (Array.isArray(value)) {
    for (const item of value) {
      const text = String(item ?? '').trim()
      if (text) return text
    }
    return ''
  }
  return String(value ?? '').trim()
}

export const readPaymentQueryValue = (query: PaymentQuery, key: string): string => {
  const normalizedKey = key.trim().toLowerCase()
  if (!normalizedKey) return ''
  const get = (name: string) => query instanceof URLSearchParams ? query.getAll(name) : query[name]
  for (const name of [key, normalizedKey, `amp;${key}`, `amp;${normalizedKey}`]) {
    const value = paymentQueryValueToString(get(name))
    if (value) return value
  }

  const entries = query instanceof URLSearchParams ? query : Object.entries(query)
  for (const [name, rawValue] of entries) {
    if (name.trim().toLowerCase().replace(/^(amp;)+/, '') !== normalizedKey) continue
    const value = paymentQueryValueToString(rawValue)
    if (value) return value
  }
  return ''
}

export const readPaymentQueryFlag = (query: PaymentQuery, key: string): boolean =>
  ['1', 'true', 'yes'].includes(readPaymentQueryValue(query, key).toLowerCase())

export const buildMemberPaymentResumePath = (search: string): string | null => {
  const query = new URLSearchParams(search)
  const orderNo = readPaymentQueryValue(query, 'order_no') || readPaymentQueryValue(query, 'out_trade_no')
  if (!/^[a-z0-9][a-z0-9_-]*$/i.test(orderNo) || readPaymentQueryFlag(query, 'guest')) return null
  if (/^WR/i.test(orderNo) || readPaymentQueryValue(query, 'biz_type').toLowerCase() === 'recharge') return null

  const resume = new URLSearchParams({ order_no: orderNo })
  for (const marker of PAYMENT_RETURN_MARKERS) {
    if (readPaymentQueryValue(query, marker) === '1') resume.set(marker, '1')
  }
  // Capture uses the payment record on the server. Carry the provider marker instead of its token.
  if (readPaymentQueryValue(query, 'token') || readPaymentQueryValue(query, 'payer_id') || readPaymentQueryValue(query, 'PayerID')) {
    resume.set('pp_return', '1')
  }
  if (readPaymentQueryValue(query, 'session_id')) resume.set('stripe_return', '1')
  return `/pay?${resume}`
}

export const isCustomerSurchargePayment = (payment?: { fee_policy?: unknown } | null) => {
  const policy = String(payment?.fee_policy || '').trim().toLowerCase()
  return policy === 'customer_surcharge' || policy === 'legacy_customer_surcharge'
}

export const isRedirectPaymentInteractionMode = (mode: unknown) => {
  return REDIRECT_PAYMENT_INTERACTION_MODES.has(normalizeInteractionMode(mode))
}

export const resolvePaymentPresentationMode = (mode: unknown): PaymentPresentationMode => {
  return isRedirectPaymentInteractionMode(mode) ? 'redirect' : 'qr'
}

export const resolvePaymentLinkNavigationTarget = (automatic: boolean): PaymentLinkNavigationTarget => {
  return automatic ? 'current-tab' : 'new-window'
}

export const resolvePaymentInteractionLabelKey = (mode: unknown): PaymentInteractionLabelKey | null => {
  switch (normalizeInteractionMode(mode)) {
    case 'qr':
      return 'payment.modeQr'
    case 'redirect':
      return 'payment.modeRedirect'
    case 'wap':
      return 'payment.modeWap'
    case 'page':
      return 'payment.modePage'
    default:
      return null
  }
}

export const resolvePaymentResultTitleKey = (mode: unknown): PaymentResultTitleKey => {
  switch (normalizeInteractionMode(mode)) {
    case 'wap':
      return 'payment.modeWap'
    case 'page':
      return 'payment.modePage'
    case 'redirect':
      return 'payment.resultRedirectTitle'
    default:
      return 'payment.resultTitle'
  }
}

export const getPaymentResetPolicy = (reason: PaymentResetReason = 'generic'): PaymentResetPolicy => {
  if (reason === 'change_payment_method') {
    return {
      resumeLatestPayment: false,
      clearSelectedChannel: true,
      stopActivePaymentWatch: true,
    }
  }

  return {
    resumeLatestPayment: true,
    clearSelectedChannel: false,
    stopActivePaymentWatch: false,
  }
}

export const getCachedPaymentRestorePolicy = (): CachedPaymentRestorePolicy => ({
  startActivePaymentWatch: true,
  autoOpenPayLink: false,
})

export const shouldAutoOpenPaymentLink = (
  payment?: { interaction_mode?: unknown; pay_url?: unknown; fee_policy?: unknown } | null,
  returningFromGateway = false,
) => {
  if (returningFromGateway || !payment || isCustomerSurchargePayment(payment)) return false
  const payURL = String(payment.pay_url || '').trim()
  return isRedirectPaymentInteractionMode(payment.interaction_mode) && payURL !== ''
}
