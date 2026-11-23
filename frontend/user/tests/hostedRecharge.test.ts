import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  directCheckoutRestriction,
  HOSTED_RECHARGE,
  isDirectFulfillmentType,
  isHostedRechargeType,
  isSingleItemDirectType,
} from '../src/utils/directPurchase.ts'
import {
  hostedRechargeStatusOrderNo,
  isHostedRechargeOrder,
  isHostedRechargeOrderFulfillment,
  isTerminalHostedRechargeStage,
  parseHostedRechargeStatus,
} from '../src/utils/hostedRechargeStatus.ts'

test('hosted recharge type helpers stay distinct from gpt_direct while sharing single-item checkout rules', () => {
  assert.equal(isHostedRechargeType(HOSTED_RECHARGE), true)
  assert.equal(isHostedRechargeType('quefa_hosted'), true)
  assert.equal(isHostedRechargeType('gpt_direct'), false)
  assert.equal(isDirectFulfillmentType(HOSTED_RECHARGE), false)
  assert.equal(isSingleItemDirectType(HOSTED_RECHARGE), true)
  assert.equal(isSingleItemDirectType('gpt_direct'), true)
  assert.equal(isSingleItemDirectType('auto'), false)

  const hostedItem = { fulfillmentType: HOSTED_RECHARGE, skuId: 12, quantity: 1, purchaseType: 'member' }
  const guestHosted = { ...hostedItem, purchaseType: 'guest' }

  assert.equal(directCheckoutRestriction([hostedItem], true, true, false), '')
  assert.equal(directCheckoutRestriction([hostedItem], true, false, false), 'directPurchase.loginRequired')
  assert.equal(directCheckoutRestriction([guestHosted], true, false, false), '')
  assert.equal(directCheckoutRestriction([guestHosted], true, false, true), 'directPurchase.checkoutRestricted')
  assert.equal(directCheckoutRestriction([hostedItem], false, true, false), 'directPurchase.checkoutRestricted')
  assert.equal(directCheckoutRestriction([{ ...hostedItem, quantity: 2 }], true, true, false), 'directPurchase.checkoutRestricted')
})

test('hosted recharge order detection and status parser handle all valid stages', () => {
  const order = {
    order_no: 'DJ-20261123-0001',
    status: 'paid',
    children: [{ items: [{ fulfillment_type: HOSTED_RECHARGE }] }],
  }
  assert.equal(isHostedRechargeOrder(order), true)
  assert.equal(isHostedRechargeOrderFulfillment(order.children[0].items), true)
  assert.equal(hostedRechargeStatusOrderNo(order), 'DJ-20261123-0001')
  assert.equal(hostedRechargeStatusOrderNo({ ...order, status: 'pending_payment' }), '')

  const parsed = parseHostedRechargeStatus({
    stage: 'waiting_submit',
    progress_stage: 'ready',
    progress_message: 'Ready for submission',
    can_open_fulfillment: true,
    updated_at: '2026-11-23T12:00:00Z',
  })
  assert.deepEqual(parsed, {
    stage: 'waiting_submit',
    progress_stage: 'ready',
    progress_message: 'Ready for submission',
    progress_updated_at: undefined,
    can_open_fulfillment: true,
    updated_at: '2026-11-23T12:00:00Z',
    completed_at: undefined,
  })

  assert.equal(parseHostedRechargeStatus({ stage: 'unknown', updated_at: '2026-11-23T12:00:00Z' }), null)
  assert.equal(parseHostedRechargeStatus({ stage: 'waiting_submit', updated_at: 'not-a-date' }), null)
  assert.equal(isTerminalHostedRechargeStage('succeeded'), true)
  assert.equal(isTerminalHostedRechargeStage('closed'), true)
  assert.equal(isTerminalHostedRechargeStage('refunded'), true)
  assert.equal(isTerminalHostedRechargeStage('recharging'), false)
})

test('storefront translations are white-labeled and contain no quefa or tibo.ink branding', () => {
  for (const locale of ['zh-CN', 'zh-TW', 'en-US']) {
    const source = readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    assert.doesNotMatch(source, /quefa/i, `${locale} must not expose quefa`)
    assert.doesNotMatch(source, /tibo\.ink/i, `${locale} must not expose tibo.ink`)
    const json = JSON.parse(source)
    assert.ok(json.hostedRecharge?.entry?.openButton, `${locale} missing openButton`)
    assert.ok(json.hostedRecharge?.progress?.waiting_submit, `${locale} missing waiting_submit`)
    assert.ok(json.hostedRecharge?.stageHints?.resubmit_required, `${locale} missing resubmit_required`)
  }
})
