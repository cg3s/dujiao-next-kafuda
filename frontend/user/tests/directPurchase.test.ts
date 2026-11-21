import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  buildDirectSkuRegions,
  directCheckoutRestriction,
  filterDirectSkusByRegion,
  getDirectSkuRegion,
  GPT_DIRECT,
  isDirectSkuAvailable,
  resolveDirectSkuIdForRegion,
  UNKNOWN_DIRECT_SKU_REGION,
} from '../src/utils/directPurchase.ts'
import { resolveCardDisplayPrice } from '../src/utils/productPricing.ts'

const availableSku = {
  id: 7,
  is_active: true,
  is_sold_out: false,
  stock_status: 'in_stock',
  auto_stock_available: 0,
  manual_stock_total: 0,
}

test('direct SKU availability follows the storefront status at zero inventory', () => {
  assert.equal(isDirectSkuAvailable(availableSku), true)
  assert.equal(isDirectSkuAvailable({ ...availableSku, stock_status: 'out_of_stock' }), false)
  assert.equal(isDirectSkuAvailable({ ...availableSku, is_sold_out: true }), false)
  assert.equal(isDirectSkuAvailable({ ...availableSku, is_active: false }), false)
  assert.equal(isDirectSkuAvailable({ ...availableSku, stock_status: undefined }), false)
  assert.equal(isDirectSkuAvailable({ ...availableSku, id: 0 }), false)
  assert.equal(isDirectSkuAvailable(null), false)
})

const direct = { fulfillmentType: GPT_DIRECT, skuId: 7, quantity: 1 }
const ordinary = { fulfillmentType: 'auto', skuId: 8, quantity: 2 }

test('direct checkout accepts one specified SKU for an authenticated main storefront buyer', () => {
  assert.equal(directCheckoutRestriction([direct], true, true, false), '')
  assert.equal(directCheckoutRestriction([direct], true, false, false), 'directPurchase.loginRequired')
  assert.equal(directCheckoutRestriction([direct], true, true, true), 'directPurchase.checkoutRestricted')
  assert.equal(directCheckoutRestriction([direct], false, true, false), 'directPurchase.checkoutRestricted')
})

test('an existing provider-prefixed checkout draft keeps the direct restrictions', () => {
  const legacy = { ...direct, fulfillmentType: 'kafuda_gpt_direct' }
  assert.equal(directCheckoutRestriction([legacy], true, true, false), '')
  assert.equal(directCheckoutRestriction([legacy], false, true, false), 'directPurchase.checkoutRestricted')
})

test('storefront translations omit the provider name and still disclose the token transfer', () => {
  for (const [locale, provider] of [['zh-CN', '直充服务商'], ['zh-TW', '直充服務商'], ['en-US', 'recharge provider']]) {
    const source = readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    assert.doesNotMatch(source, /kafuda/i, locale)
    assert.ok(JSON.parse(source).directPurchase.authorization.privacy.includes(provider), locale)
  }
})

test('direct checkout rejects mixed lines, quantities other than one, and missing SKU IDs', () => {
  for (const items of [
    [direct, ordinary],
    [direct, direct],
    [{ ...direct, quantity: 2 }],
    [{ ...direct, quantity: 0 }],
    [{ ...direct, skuId: undefined }],
    [{ ...direct, skuId: 0 }],
    [{ ...direct, skuId: 0.5 }],
  ]) {
    assert.equal(directCheckoutRestriction(items, true, true, false), 'directPurchase.checkoutRestricted')
  }
})

test('ordinary product checkout keeps its existing modes', () => {
  assert.equal(directCheckoutRestriction([ordinary], false, false, false), '')
  assert.equal(directCheckoutRestriction([ordinary], true, true, true), '')
  assert.equal(directCheckoutRestriction([], true, true, false), '')
})

test('direct product cards calculate member pricing only for available SKUs', () => {
  const unavailable = { ...availableSku, id: 8, stock_status: 'out_of_stock', price_amount: '20.00' }
  const product = { fulfillment_type: GPT_DIRECT, price_amount: '100.00', skus: [unavailable, { ...availableSku, price_amount: '100.00' }] }
  assert.equal(resolveCardDisplayPrice(product, 3, 80), '80.00')
  assert.equal(resolveCardDisplayPrice({ ...product, skus: [unavailable] }, 3, 80), null)
})

test('Classic and Vault product browsing use shared availability labels', () => {
  for (const view of [
    'components/ProductCard.vue',
    'templates/vault/components/VaultProductCard.vue',
    'templates/vault/components/VaultProductListItem.vue',
  ]) {
    const source = readFileSync(new URL(`../src/${view}`, import.meta.url), 'utf8')
    assert.match(source, /getStockStatusLabel\(/, view)
    assert.doesNotMatch(source, /products\.stockStatus\.outOfStock/, view)
  }
})

test('direct SKU regions normalize, deduplicate, and localize region labels', () => {
  const skus = [
    { ...availableSku, id: 1, spec_values: { plan: 'Plus', country: ' us ' } },
    { ...availableSku, id: 2, spec_values: { plan: 'Pro', country: 'US' } },
    { ...availableSku, id: 3, spec_values: { plan: 'Plus', country: 'jp' } },
    { ...availableSku, id: 4, spec_values: { plan: 'Plus', country: 'GLOBAL' } },
    { ...availableSku, id: 5, spec_values: { plan: 'Plus' } },
  ]
  assert.equal(getDirectSkuRegion(skus[0]), 'US')
  assert.equal(getDirectSkuRegion(skus[4]), UNKNOWN_DIRECT_SKU_REGION)

  const zhRegions = buildDirectSkuRegions(skus, 'zh-CN', '未指定地区')
  assert.deepEqual(
    zhRegions.map((item) => item.value),
    ['US', 'JP', 'GLOBAL', UNKNOWN_DIRECT_SKU_REGION],
  )
  assert.equal(zhRegions[0]?.label, new Intl.DisplayNames(['zh-CN'], { type: 'region' }).of('US'))
  assert.equal(zhRegions[1]?.label, new Intl.DisplayNames(['zh-CN'], { type: 'region' }).of('JP'))
  assert.equal(zhRegions[2]?.label, 'GLOBAL')
  assert.equal(zhRegions[3]?.label, '未指定地区')

  assert.deepEqual(
    filterDirectSkusByRegion(skus, 'US').map((sku) => sku.id),
    [1, 2],
  )
})

test('resolveDirectSkuIdForRegion prefers the first available SKU in the selected region', () => {
  const skus = [
    { ...availableSku, id: 10, stock_status: 'out_of_stock', spec_values: { plan: 'Plus', country: 'US' } },
    { ...availableSku, id: 11, stock_status: 'in_stock', spec_values: { plan: 'Pro', country: 'US' } },
    { ...availableSku, id: 12, stock_status: 'out_of_stock', spec_values: { plan: 'Plus', country: 'JP' } },
  ]
  const normalize = (id: unknown) => Number(id) || 0
  assert.equal(resolveDirectSkuIdForRegion(skus, 'US', normalize), 11)
  assert.equal(resolveDirectSkuIdForRegion(skus, 'JP', normalize), 12)
  assert.equal(resolveDirectSkuIdForRegion(skus, 'GB', normalize), 0)
})

test('Classic and Vault product detail pages render direct SKU region tabs and visible SKUs', () => {
  for (const view of [
    'views/ProductDetail.vue',
    'templates/vault/ProductDetail.vue',
  ]) {
    const source = readFileSync(new URL(`../src/${view}`, import.meta.url), 'utf8')
    assert.match(source, /directSkuRegions\.length/, view)
    assert.match(source, /selectSkuRegion\(region\.value\)/, view)
    assert.match(source, /v-for="sku in visibleSkus"/, view)
  }
  for (const locale of ['zh-CN', 'zh-TW', 'en-US']) {
    const data = JSON.parse(readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8'))
    assert.ok(data.directPurchase.skuRegions, `${locale}: skuRegions`)
    assert.ok(data.directPurchase.unknownRegion, `${locale}: unknownRegion`)
  }
})
