import type { CartItem } from '../stores/cart'

export const KAFUDA_GPT_DIRECT = 'kafuda_gpt_direct'

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
  const direct = items.find((item) => item.fulfillmentType === KAFUDA_GPT_DIRECT)
  if (!direct) return ''
  if (!buyNow || reseller || items.length !== 1 || !isPositiveSkuId(direct.skuId) || direct.quantity !== 1) {
    return 'directPurchase.checkoutRestricted'
  }
  return authenticated ? '' : 'directPurchase.loginRequired'
}
