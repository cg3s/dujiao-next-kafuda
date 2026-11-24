export interface QuefaCatalogLike {
  product_code?: string
  sku_code?: string
  payment_country?: string
  fulfillment_mode?: string
  delivery_modes?: string[]
  quote_required?: boolean
  spec_values?: Record<string, string>
}

export const QUEFA_UNKNOWN_GROUP = '__unknown__'

export function resolveQuefaProductCode(item?: QuefaCatalogLike | null): string {
  const fromSpec = String(item?.spec_values?.product_code || '').trim()
  if (fromSpec) return fromSpec.toLowerCase()
  const fromCode = String(item?.product_code || '').trim()
  if (fromCode) return fromCode.toLowerCase()
  return String(item?.sku_code || '').trim().toLowerCase()
}

export function resolveQuefaCountry(item?: QuefaCatalogLike | null): string {
  const raw = String(item?.payment_country || item?.spec_values?.country || '').trim().toUpperCase()
  return raw || ''
}

export function isQuefaChannel2Item(item?: QuefaCatalogLike | null): boolean {
  const code = resolveQuefaProductCode(item)
  if (code.includes('pro20x')) return true
  const mode = String(item?.fulfillment_mode || item?.spec_values?.fulfillment_mode || '').trim().toLowerCase()
  const country = resolveQuefaCountry(item)
  return mode === 'direct' && country === 'PH' && !code.includes('codex')
}

export function resolveQuefaGroupKey(item?: QuefaCatalogLike | null): string {
  const code = resolveQuefaProductCode(item)
  if (code.startsWith('codex') || code.includes('_codex')) return 'codex'
  if (isQuefaChannel2Item(item)) return 'channel2'
  const country = resolveQuefaCountry(item)
  if (country === 'PH') return 'ph'
  if (country === 'EG') return 'egypt'
  if (country) return country
  return QUEFA_UNKNOWN_GROUP
}

export function resolveQuefaGroupLabel(
  key: string,
  locale: string,
  labels: { ph: string; channel2: string; egypt: string; codex: string; other: string },
): string {
  if (key === 'ph') return labels.ph
  if (key === 'channel2') return labels.channel2
  if (key === 'egypt') return labels.egypt
  if (key === 'codex') return labels.codex
  if (!key || key === QUEFA_UNKNOWN_GROUP) return labels.other
  if (/^[A-Z]{2}$/.test(key)) {
    try {
      return new Intl.DisplayNames([locale], { type: 'region' }).of(key) || key
    } catch {
      return key
    }
  }
  return key
}

export function sortQuefaGroupKeys(keys: Iterable<string>): string[] {
  const priority = ['ph', 'channel2', 'codex', 'egypt']
  return Array.from(keys).sort((a, b) => {
    if (a === QUEFA_UNKNOWN_GROUP) return 1
    if (b === QUEFA_UNKNOWN_GROUP) return -1
    const ia = priority.indexOf(a)
    const ib = priority.indexOf(b)
    if (ia !== -1 || ib !== -1) {
      return (ia === -1 ? 999 : ia) - (ib === -1 ? 999 : ib)
    }
    return a.localeCompare(b)
  })
}

export function resolveQuefaDeliveryModeKey(item?: QuefaCatalogLike | null): 'cdk_or_auto' | 'channel2_direct' | 'auto_only' {
  if (isQuefaChannel2Item(item)) return 'channel2_direct'
  const modes = Array.isArray(item?.delivery_modes) ? item!.delivery_modes.map((m) => String(m).toLowerCase()) : []
  if (modes.includes('cdk') && modes.includes('auto_recharge')) return 'cdk_or_auto'
  const fm = String(item?.fulfillment_mode || item?.spec_values?.fulfillment_mode || '').toLowerCase()
  if (fm === 'cdk') return 'cdk_or_auto'
  return 'auto_only'
}
