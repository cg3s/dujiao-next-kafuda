import type { CartItem } from '../stores/cart'

export const GPT_DIRECT = 'gpt_direct'

// Older checkout drafts and storefront APIs used a provider-prefixed value.
export const isDirectFulfillmentType = (type: unknown): boolean =>
  typeof type === 'string' && (type === GPT_DIRECT || type.endsWith(`_${GPT_DIRECT}`))

const isPositiveSkuId = (id: unknown): id is number =>
  typeof id === 'number' && Number.isSafeInteger(id) && id > 0

export const isDirectSkuAvailable = (sku?: {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
} | null) => Boolean(sku?.is_active && isPositiveSkuId(sku.id) && !sku.is_sold_out && sku.stock_status === 'in_stock')

export const directCheckoutRestriction = (
  items: readonly Pick<CartItem, 'fulfillmentType' | 'skuId' | 'quantity'>[],
  buyNow: boolean,
  authenticated: boolean,
  reseller: boolean,
) => {
  const direct = items.find((item) => isDirectFulfillmentType(item.fulfillmentType))
  if (!direct) return ''
  if (!buyNow || reseller || items.length !== 1 || !isPositiveSkuId(direct.skuId) || direct.quantity !== 1) {
    return 'directPurchase.checkoutRestricted'
  }
  return authenticated ? '' : 'directPurchase.loginRequired'
}
