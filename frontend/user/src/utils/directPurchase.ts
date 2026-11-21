import type { CartItem } from '../stores/cart'

export const GPT_DIRECT = 'gpt_direct'
export const UNKNOWN_DIRECT_SKU_REGION = '__unknown__'

export interface DirectSkuRegionOption {
  value: string
  label: string
}

// Older checkout drafts and storefront APIs used a provider-prefixed value.
export const isDirectFulfillmentType = (type: unknown): boolean =>
  typeof type === 'string' && (type === GPT_DIRECT || type.endsWith(`_${GPT_DIRECT}`))

export const getDirectSkuRegion = (sku?: {
  spec_values?: Record<string, unknown> | null
} | null): string => {
  const raw = typeof sku?.spec_values?.country === 'string' ? sku.spec_values.country.trim().toUpperCase() : ''
  return raw || UNKNOWN_DIRECT_SKU_REGION
}

export const buildDirectSkuRegions = (
  skus: readonly { spec_values?: Record<string, unknown> | null }[],
  locale: string,
  unknownLabel: string,
): DirectSkuRegionOption[] => {
  const regions = new Set(skus.map((sku) => getDirectSkuRegion(sku)))
  let displayNames: Intl.DisplayNames | null = null
  try {
    displayNames = new Intl.DisplayNames([locale], { type: 'region' })
  } catch {
    displayNames = null
  }
  return Array.from(regions, (region) => ({
    value: region,
    label: region === UNKNOWN_DIRECT_SKU_REGION
      ? unknownLabel
      : /^[A-Z]{2}$/.test(region) ? (displayNames?.of(region) || region) : region,
  }))
}

export const filterDirectSkusByRegion = <T extends { spec_values?: Record<string, unknown> | null }>(
  skus: readonly T[],
  region: string,
): T[] => skus.filter((sku) => getDirectSkuRegion(sku) === region)

const isPositiveSkuId = (id: unknown): id is number =>
  typeof id === 'number' && Number.isSafeInteger(id) && id > 0

export const isDirectSkuAvailable = (sku?: {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
} | null) => Boolean(sku?.is_active && isPositiveSkuId(sku.id) && !sku.is_sold_out && sku.stock_status === 'in_stock')

export const resolveDirectSkuIdForRegion = <T extends {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
  spec_values?: Record<string, unknown> | null
}>(
  skus: readonly T[],
  region: string,
  normalizeId: (id: unknown) => number,
): number => {
  const inRegion = filterDirectSkusByRegion(skus, region)
  if (inRegion.length === 0) return 0
  const firstAvailable = inRegion.find((sku) => isDirectSkuAvailable(sku))
  return normalizeId((firstAvailable ?? inRegion[0])?.id)
}

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
