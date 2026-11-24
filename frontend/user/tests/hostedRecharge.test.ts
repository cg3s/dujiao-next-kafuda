import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  buildHostedRechargeSkuGroups,
  directCheckoutRestriction,
  filterHostedRechargeSkusByGroup,
  getHostedRechargeSkuGroup,
  HOSTED_RECHARGE,
  isDirectFulfillmentType,
  isHostedChannel2Sku,
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
    account_email_masked: 'u***@example.com',
    attempt_no: 1,
    paused: false,
    retry_allowed: false,
    next_action: 'wait',
    can_submit_redemption: true,
    can_open_fulfillment: true,
    has_hosted_entry: true,
    updated_at: '2026-11-23T12:00:00Z',
  })
  assert.deepEqual(parsed, {
    stage: 'waiting_submit',
    progress_stage: 'ready',
    progress_message: 'Ready for submission',
    progress_updated_at: undefined,
    account_email_masked: 'u***@example.com',
    attempt_no: 1,
    paused: false,
    retry_allowed: false,
    next_action: 'wait',
    error_category: undefined,
    failure_code: undefined,
    can_submit_redemption: true,
    can_open_fulfillment: true,
    has_hosted_entry: true,
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

test('hosted recharge SKU grouping mirrors official channel/region categories', () => {
  const skus = [
    { id: 1, sku_code: 'chatgpt_plus_cdk_1m', spec_values: { product_code: 'chatgpt_plus_cdk_1m', country: 'PH' } },
    { id: 2, sku_code: 'chatgpt_plus_pro20x_ph_1m', spec_values: { product_code: 'chatgpt_plus_pro20x_ph_1m', country: 'PH', fulfillment_mode: 'direct' } },
    { id: 3, sku_code: 'codex_250', spec_values: { product_code: 'codex_250', country: 'PH' } },
    { id: 4, sku_code: 'codex_500', spec_values: { product_code: 'codex_500', country: 'PH' } },
  ]
  assert.equal(getHostedRechargeSkuGroup(skus[0]), 'ph')
  assert.equal(getHostedRechargeSkuGroup(skus[1]), 'channel2')
  assert.equal(isHostedChannel2Sku(skus[1]), true)
  assert.equal(getHostedRechargeSkuGroup(skus[2]), 'codex')

  const groups = buildHostedRechargeSkuGroups(skus, 'zh-CN', {
    ph: '菲律宾充值',
    channel2: '渠道二直充',
    egypt: '埃及订阅',
    codex: 'Codex 点数',
    other: '其他地区',
  })
  assert.deepEqual(groups, [
    { value: 'ph', label: '菲律宾充值 (1)' },
    { value: 'channel2', label: '渠道二直充 (1)' },
    { value: 'codex', label: 'Codex 点数 (2)' },
  ])
  assert.equal(filterHostedRechargeSkusByGroup(skus, 'codex').length, 2)
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
