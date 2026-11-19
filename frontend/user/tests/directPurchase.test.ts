import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { directCheckoutRestriction, isDirectSkuAvailable, KAFUDA_GPT_DIRECT } from '../src/utils/directPurchase.ts'
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

const direct = { fulfillmentType: KAFUDA_GPT_DIRECT, skuId: 7, quantity: 1 }
const ordinary = { fulfillmentType: 'auto', skuId: 8, quantity: 2 }

test('direct checkout accepts one specified SKU for an authenticated main storefront buyer', () => {
  assert.equal(directCheckoutRestriction([direct], true, true, false), '')
  assert.equal(directCheckoutRestriction([direct], true, false, false), 'directPurchase.loginRequired')
  assert.equal(directCheckoutRestriction([direct], true, true, true), 'directPurchase.checkoutRestricted')
  assert.equal(directCheckoutRestriction([direct], false, true, false), 'directPurchase.checkoutRestricted')
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
  const product = { fulfillment_type: KAFUDA_GPT_DIRECT, price_amount: '100.00', skus: [unavailable, { ...availableSku, price_amount: '100.00' }] }
  assert.equal(resolveCardDisplayPrice(product, 3, 80), '80.00')
  assert.equal(resolveCardDisplayPrice({ ...product, skus: [unavailable] }, 3, 80), null)
})

test('Classic and Vault product browsing use shared availability labels', () => {
  for (const view of [
    'components/ProductCard.vue',
    'components/ProductListItem.vue',
    'templates/vault/components/VaultProductCard.vue',
    'templates/vault/components/VaultProductListItem.vue',
  ]) {
    const source = readFileSync(new URL(`../src/${view}`, import.meta.url), 'utf8')
    assert.match(source, /getStockStatusLabel\(/, view)
    assert.doesNotMatch(source, /products\.stockStatus\.outOfStock/, view)
  }
})
