import type { CartItem } from '../stores/cart'
import { amountToCents } from './money.ts'

export const GPT_DIRECT = 'gpt_direct'
export const HOSTED_RECHARGE = 'hosted_recharge'
export const UNKNOWN_DIRECT_SKU_REGION = '__unknown__'

export interface DirectSkuRegionOption {
  value: string
  label: string
}

export interface DirectSkuSpecOption {
  value: string
  label: string
}

// Older checkout drafts and storefront APIs used a provider-prefixed value.
export const isDirectFulfillmentType = (type: unknown): boolean =>
  typeof type === 'string' && (type === GPT_DIRECT || type.endsWith(`_${GPT_DIRECT}`))

export const isHostedRechargeType = (type: unknown): boolean =>
  typeof type === 'string' && (type === HOSTED_RECHARGE || type === 'quefa_hosted')

export const isSingleItemDirectType = (type: unknown): boolean =>
  isDirectFulfillmentType(type) || isHostedRechargeType(type)

export const getDirectSkuRegion = (sku?: {
  spec_values?: Record<string, unknown> | null
} | null): string => {
  const raw = typeof sku?.spec_values?.country === 'string' ? sku.spec_values.country.trim().toUpperCase() : ''
  return raw || UNKNOWN_DIRECT_SKU_REGION
}

type HostedRechargeSkuLike = {
  sku_code?: unknown
  spec_values?: Record<string, unknown> | null
}

const getHostedProductCode = (sku?: HostedRechargeSkuLike | null): string => {
  const fromSpec = typeof sku?.spec_values?.product_code === 'string' ? sku.spec_values.product_code.trim() : ''
  if (fromSpec) return fromSpec.toLowerCase()
  return typeof sku?.sku_code === 'string' ? sku.sku_code.trim().toLowerCase() : ''
}

export const isHostedChannel2Sku = (sku?: HostedRechargeSkuLike | null): boolean => {
  const code = getHostedProductCode(sku)
  if (code.includes('pro20x')) return true
  const mode = typeof sku?.spec_values?.fulfillment_mode === 'string' ? sku.spec_values.fulfillment_mode.trim().toLowerCase() : ''
  const region = getDirectSkuRegion(sku)
  return mode === 'direct' && region === 'PH' && !code.includes('codex')
}

export const getHostedRechargeSkuGroup = (sku?: HostedRechargeSkuLike | null): string => {
  const code = getHostedProductCode(sku)
  if (code.startsWith('codex') || code.includes('_codex')) return 'codex'
  if (isHostedChannel2Sku(sku)) return 'channel2'
  const region = getDirectSkuRegion(sku)
  if (region === 'PH') return 'ph'
  if (region === 'EG') return 'egypt'
  return region
}

export const buildHostedRechargeSkuGroups = (
  skus: readonly HostedRechargeSkuLike[],
  locale: string,
  labels: { ph: string; channel2: string; egypt: string; codex: string; other: string },
): DirectSkuSpecOption[] => {
  const counts = new Map<string, number>()
  for (const sku of skus) {
    const group = getHostedRechargeSkuGroup(sku)
    counts.set(group, (counts.get(group) ?? 0) + 1)
  }
  if (counts.size <= 1) return []
  const order = ['ph', 'channel2', 'codex', 'egypt']
  const keys = Array.from(counts.keys()).sort((a, b) => {
    const ia = order.indexOf(a)
    const ib = order.indexOf(b)
    if (ia !== -1 || ib !== -1) {
      return (ia === -1 ? 999 : ia) - (ib === -1 ? 999 : ib)
    }
    return a.localeCompare(b)
  })
  return keys.map((key) => {
    const count = counts.get(key) ?? 0
    let name: string
    if (key === 'ph') name = labels.ph
    else if (key === 'channel2') name = labels.channel2
    else if (key === 'egypt') name = labels.egypt
    else if (key === 'codex') name = labels.codex
    else name = formatDirectSkuRegionLabel(key, locale, labels.other)
    return { value: key, label: `${name} (${count})` }
  })
}

export const filterHostedRechargeSkusByGroup = <T extends HostedRechargeSkuLike>(
  skus: readonly T[],
  group: string,
): T[] => skus.filter((sku) => getHostedRechargeSkuGroup(sku) === group)

export const resolveHostedRechargeSkuIdForGroup = <T extends HostedRechargeSkuLike & {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
  price_amount?: unknown
}>(
  skus: readonly T[],
  group: string,
  options: {
    getPriceCents?: (sku: T) => number | null
    normalizeId: (id: unknown) => number
  },
): number => {
  const inGroup = filterHostedRechargeSkusByGroup(skus, group)
  if (inGroup.length === 0) return 0
  const sorted = sortDirectSkusByPrice(inGroup, options.getPriceCents)
  return options.normalizeId(sorted[0]?.id)
}

export const formatDirectSkuRegionLabel = (
  region: string,
  locale: string,
  unknownLabel: string,
): string => {
  if (!region || region === UNKNOWN_DIRECT_SKU_REGION) return unknownLabel
  if (!/^[A-Z]{2}$/.test(region)) return region
  try {
    return new Intl.DisplayNames([locale], { type: 'region' }).of(region) || region
  } catch {
    return region
  }
}

export const buildDirectSkuRegions = (
  skus: readonly { spec_values?: Record<string, unknown> | null }[],
  locale: string,
  unknownLabel: string,
): DirectSkuRegionOption[] => {
  const regions = new Set(skus.map((sku) => getDirectSkuRegion(sku)))
  return Array.from(regions, (region) => ({
    value: region,
    label: formatDirectSkuRegionLabel(region, locale, unknownLabel),
  }))
}

export const filterDirectSkusByRegion = <T extends { spec_values?: Record<string, unknown> | null }>(
  skus: readonly T[],
  region: string,
): T[] => skus.filter((sku) => getDirectSkuRegion(sku) === region)

export const buildDirectSkuSpecs = <T>(
  skus: readonly T[],
  getSpecName: (sku: T) => string,
): DirectSkuSpecOption[] => {
  const seen = new Set<string>()
  const result: DirectSkuSpecOption[] = []
  for (const sku of skus) {
    const label = getSpecName(sku).trim()
    if (!label || seen.has(label)) continue
    seen.add(label)
    result.push({ value: label, label })
  }
  return result
}

export const filterDirectSkusBySpec = <T>(
  skus: readonly T[],
  specName: string,
  getSpecName: (sku: T) => string,
): T[] => {
  const target = specName.trim()
  return skus.filter((sku) => getSpecName(sku).trim() === target)
}

export const sortDirectSkusByPrice = <T extends {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
  price_amount?: unknown
}>(
  skus: readonly T[],
  getPriceCents?: (sku: T) => number | null,
): T[] => {
  const resolvePrice = getPriceCents ?? ((sku: T) => amountToCents(sku.price_amount))
  return skus
    .map((sku, index) => {
      const cents = resolvePrice(sku)
      return {
        sku,
        index,
        available: isDirectSkuAvailable(sku) ? 0 : 1,
        price: cents !== null && cents >= 0 ? cents : Number.POSITIVE_INFINITY,
      }
    })
    .sort((a, b) => (a.available - b.available) || (a.price - b.price) || (a.index - b.index))
    .map((entry) => entry.sku)
}

export const resolveDirectSkuIdForSpec = <T extends {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
  price_amount?: unknown
}>(
  skus: readonly T[],
  specName: string,
  options: {
    getSpecName: (sku: T) => string
    getPriceCents?: (sku: T) => number | null
    normalizeId: (id: unknown) => number
  },
): number => {
  const inSpec = filterDirectSkusBySpec(skus, specName, options.getSpecName)
  if (inSpec.length === 0) return 0
  const sorted = sortDirectSkusByPrice(inSpec, options.getPriceCents)
  return options.normalizeId(sorted[0]?.id)
}

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

export const findLowestPriceDirectSkuIds = <T extends {
  id?: unknown
  is_active?: boolean
  is_sold_out?: boolean
  stock_status?: string
  price_amount?: unknown
  spec_values?: Record<string, unknown> | null
}>(
  skus: readonly T[],
  options: {
    getSpecName: (sku: T) => string
    getPriceCents?: (sku: T) => number | null
    normalizeId: (id: unknown) => number
  },
): Set<number> => {
  const groups = new Map<string, Array<{ id: number; region: string; priceCents: number }>>()
  const resolvePrice = options.getPriceCents ?? ((sku: T) => amountToCents(sku.price_amount))
  for (const sku of skus) {
    if (!isDirectSkuAvailable(sku)) continue
    const id = options.normalizeId(sku.id)
    if (id <= 0) continue
    const specName = options.getSpecName(sku).trim()
    if (!specName) continue
    const priceCents = resolvePrice(sku)
    if (priceCents === null || priceCents < 0) continue
    const region = getDirectSkuRegion(sku)
    const bucket = groups.get(specName)
    if (bucket) {
      bucket.push({ id, region, priceCents })
    } else {
      groups.set(specName, [{ id, region, priceCents }])
    }
  }

  const lowestIds = new Set<number>()
  for (const entries of groups.values()) {
    const regions = new Set(entries.map((entry) => entry.region))
    if (regions.size < 2) continue
    let minPrice = Number.POSITIVE_INFINITY
    let maxPrice = Number.NEGATIVE_INFINITY
    for (const entry of entries) {
      if (entry.priceCents < minPrice) minPrice = entry.priceCents
      if (entry.priceCents > maxPrice) maxPrice = entry.priceCents
    }
    if (minPrice >= maxPrice) continue
    for (const entry of entries) {
      if (entry.priceCents === minPrice) {
        lowestIds.add(entry.id)
      }
    }
  }
  return lowestIds
}

export const directCheckoutRestriction = (
  items: readonly Pick<CartItem, 'fulfillmentType' | 'skuId' | 'quantity' | 'purchaseType'>[],
  buyNow: boolean,
  authenticated: boolean,
  reseller: boolean,
) => {
  const direct = items.find((item) => isSingleItemDirectType(item.fulfillmentType))
  if (!direct) return ''
  if (!buyNow || reseller || items.length !== 1 || !isPositiveSkuId(direct.skuId) || direct.quantity !== 1) {
    return 'directPurchase.checkoutRestricted'
  }
  if (authenticated || direct.purchaseType === 'guest') {
    return ''
  }
  return 'directPurchase.loginRequired'
}
