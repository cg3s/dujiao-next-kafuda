import test from 'node:test'
import assert from 'node:assert/strict'
import { resolveAuthFailureLoginPath, shouldClearExpiredUserAuth } from '../src/utils/authEndpoints.ts'
import {
  getCachedPaymentRestorePolicy,
  getPaymentResetPolicy,
  isCustomerSurchargePayment,
  isRedirectPaymentInteractionMode,
  resolvePaymentInteractionLabelKey,
  resolvePaymentLinkNavigationTarget,
  resolvePaymentPresentationMode,
  resolvePaymentResultTitleKey,
  shouldAutoOpenPaymentLink,
} from '../src/utils/paymentResumePolicy.ts'

test('manual payment method changes do not auto resume the latest payment', () => {
  assert.deepEqual(getPaymentResetPolicy('change_payment_method'), {
    resumeLatestPayment: false,
    clearSelectedChannel: true,
    stopActivePaymentWatch: true,
  })
})

test('route changes keep the normal latest payment resume behavior', () => {
  assert.deepEqual(getPaymentResetPolicy('route_change'), {
    resumeLatestPayment: true,
    clearSelectedChannel: false,
    stopActivePaymentWatch: false,
  })
})

test('cached payment restore resumes status watching without opening a cashier', () => {
  assert.deepEqual(getCachedPaymentRestorePolicy(), {
    startActivePaymentWatch: true,
    autoOpenPayLink: false,
  })
})

test('resuming a redirect payment after a gateway return keeps the cashier closed', () => {
  const payment = { interaction_mode: 'redirect', pay_url: 'https://pay.example.com' }
  assert.equal(shouldAutoOpenPaymentLink(payment, true), false)
  assert.equal(shouldAutoOpenPaymentLink(payment), true)
})

test('an expired member payment session resumes the same order with only recognized return parameters', () => {
  const login = resolveAuthFailureLoginPath('/pay', '?amp;order_no=IGNORED&order_no=DJ20261119&stripe_return=1&session_id=gateway-session&redirect=https%3A%2F%2Fexample.com')
  const url = new URL(login!, 'https://storefront.example')
  assert.equal(url.pathname, '/auth/login')
  assert.equal(url.searchParams.get('redirect'), '/pay?order_no=DJ20261119&stripe_return=1')

  const fallback = resolveAuthFailureLoginPath('/pay/', '?amp;OUT_TRADE_NO=DJ-LEGACY-7&token=gateway-token&PayerID=opaque-id')
  assert.equal(new URL(fallback!, url).searchParams.get('redirect'), '/pay?order_no=DJ-LEGACY-7&pp_return=1')
})

test('unexpected auth redirects do not resume guest, recharge, or external targets', () => {
  assert.equal(resolveAuthFailureLoginPath('/pay', '?order_no=DJ20261119&guest=yes'), '/auth/login')
  assert.equal(resolveAuthFailureLoginPath('/pay', '?order_no=WR20261119&biz_type=recharge'), '/auth/login')
  assert.equal(resolveAuthFailureLoginPath('/pay', '?order_no=https%3A%2F%2Fexample.com'), '/auth/login')
  assert.equal(resolveAuthFailureLoginPath('//example.com/pay', '?order_no=DJ20261119'), '/auth/login')
  assert.equal(resolveAuthFailureLoginPath('/auth/login/', '?redirect=%2Fpay'), null)
})

test('auth failure applies only to the credentials used by the failing request', () => {
  assert.equal(shouldClearExpiredUserAuth('/orders/DJ20261119', 'expired-session', 'expired-session'), true)
  assert.equal(shouldClearExpiredUserAuth('/orders/DJ20261119', null, null), true)
  assert.equal(shouldClearExpiredUserAuth('/orders/DJ20261119', 'expired-session', 'current-session'), false)
  assert.equal(shouldClearExpiredUserAuth('/orders/DJ20261119', null, 'current-session'), false)
  assert.equal(shouldClearExpiredUserAuth('/guest/orders/DJ20261119', undefined, 'current-session'), false)
  assert.equal(shouldClearExpiredUserAuth('/auth/login', null, null), false)
})

test('Alipay page and WAP modes use redirect presentation while QR stays scannable', () => {
  assert.equal(resolvePaymentPresentationMode('qr'), 'qr')
  assert.equal(resolvePaymentPresentationMode('redirect'), 'redirect')
  assert.equal(resolvePaymentPresentationMode('wap'), 'redirect')
  assert.equal(resolvePaymentPresentationMode('page'), 'redirect')
  assert.equal(isRedirectPaymentInteractionMode(' WAP '), true)
  assert.equal(isRedirectPaymentInteractionMode('qr'), false)
  assert.equal(resolvePaymentInteractionLabelKey('qr'), 'payment.modeQr')
  assert.equal(resolvePaymentInteractionLabelKey('wap'), 'payment.modeWap')
  assert.equal(resolvePaymentInteractionLabelKey('page'), 'payment.modePage')
  assert.equal(resolvePaymentResultTitleKey('qr'), 'payment.resultTitle')
  assert.equal(resolvePaymentResultTitleKey('wap'), 'payment.modeWap')
  assert.equal(resolvePaymentResultTitleKey('page'), 'payment.modePage')
})

test('redirect-style payments auto open unless a customer fee needs confirmation', () => {
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'redirect', pay_url: 'https://pay.example.com' }),
    true,
  )
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'wap', pay_url: 'https://pay.example.com/alipay-wap' }),
    true,
  )
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'page', pay_url: 'https://pay.example.com/alipay-page' }),
    true,
  )
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'qr', pay_url: 'https://pay.example.com' }),
    false,
  )
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'redirect', pay_url: '   ' }),
    false,
  )
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'redirect', pay_url: 'https://pay.example.com', fee_policy: 'customer_surcharge' }),
    false,
  )
  assert.equal(
    shouldAutoOpenPaymentLink({ interaction_mode: 'redirect', pay_url: 'https://pay.example.com', fee_policy: 'legacy_customer_surcharge' }),
    false,
  )
  assert.equal(isCustomerSurchargePayment({ fee_policy: 'merchant_absorbed' }), false)
  assert.equal(isCustomerSurchargePayment({ fee_policy: 'legacy_customer_surcharge' }), true)
})

test('automatic cashier navigation uses the current tab to avoid popup blocking', () => {
  assert.equal(resolvePaymentLinkNavigationTarget(true), 'current-tab')
  assert.equal(resolvePaymentLinkNavigationTarget(false), 'new-window')
})
