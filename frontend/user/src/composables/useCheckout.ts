import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useCartStore, type CartItem } from '../stores/cart'
import { useBuyNowStore } from '../stores/buyNow'
import {
  directCheckoutRestriction,
  GPT_DIRECT,
  HOSTED_RECHARGE,
  isDirectFulfillmentType,
  isHostedRechargeType,
  isSingleItemDirectType,
} from '../utils/directPurchase'
import { useAppStore } from '../stores/app'
import { useUserAuthStore } from '../stores/userAuth'
import { guestOrderAPI, userOrderAPI, walletAPI, type CaptchaPayload } from '../api'
import { ApiRequestError } from '../api/client'
import { debounceAsync } from '../utils/debounce'
import { type PageAlert } from '../utils/alerts'
import { amountToCents, basisPointsToPercent, centsToAmount, parseInteger, rateToBasisPoints } from '../utils/money'
import { buildSkuDisplayText, normalizeSkuId } from '../utils/sku'
import { refreshCartStockSnapshots, cartItemPurchaseLimit as itemPurchaseLimit, cartItemPurchaseMin as itemPurchaseMin } from '../utils/cartStock'
import { getImageUrl } from '../utils/image'
import { getAffiliateCode, getAffiliateVisitorKey } from '../utils/affiliate'
import { saveGuestOrderAuth } from '../utils/guestOrderAuth'
import ImageCaptcha from '../components/captcha/ImageCaptcha.vue'
import TurnstileCaptcha from '../components/captcha/TurnstileCaptcha.vue'
import { useLocalized, useProductLabels } from './useProduct'

interface ManualFormField {
  key: string
  type: string
  required: boolean
  label?: Record<string, string>
  placeholder?: Record<string, string>
  regex?: string
  min?: number
  max?: number
  max_len?: number
  options: string[]
}

interface ManualFormProduct {
  itemKey: string
  productId: number
  title: any
  fields: ManualFormField[]
  skuCount: number
}

/**
 * 结算页共享逻辑（classic + vault 双模板共用）。
 * 完整保留原 views/Checkout.vue 的行为，仅抽离为 composable。
 */
export function useCheckout() {
  const router = useRouter()
  const route = useRoute()
  const cartStore = useCartStore()
  const buyNowStore = useBuyNowStore()
  const appStore = useAppStore()
  const userAuthStore = useUserAuthStore()
  const { t } = useI18n()
  const sessionToken = userAuthStore.token
  const reloading = ref(false)

  const { getLocalizedText, siteCurrency, formatPrice } = useLocalized()
  const { resolveWholesalePriceAmount } = useProductLabels()

  const isBuyNowMode = computed(() => route.query.mode === 'buynow')
  const cartItems = computed<CartItem[]>(() => {
    if (isBuyNowMode.value) {
      return buyNowStore.item ? [buyNowStore.item] : []
    }
    return cartStore.items
  })
  const hasDirectItems = computed(() => cartItems.value.some((item) => isSingleItemDirectType(item.fulfillmentType)))
  const hasHostedRechargeItems = computed(() => cartItems.value.some((item) => isHostedRechargeType(item.fulfillmentType)))
  const checkoutLoginTo = computed(() => hasDirectItems.value
    ? { path: '/auth/login', query: { redirect: route.fullPath } }
    : '/auth/login')
  const directCheckoutError = computed(() => directCheckoutRestriction(
    cartItems.value, isBuyNowMode.value, userAuthStore.isAuthenticated, appStore.isResellerTenant,
  ))
  const totalItems = computed(() => cartItems.value.reduce((sum, item) => sum + item.quantity, 0))
  const couponCode = ref('')
  const normalizedCouponCode = computed(() => couponCode.value.trim())
  const submitting = ref(false)
  const error = ref('')
  const preview = ref<any>(null)
  const previewLoading = ref(false)
  const previewError = ref('')
  const directPreviewRetryable = ref(false)
  const previewRequestId = ref(0)
  const couponRefreshing = ref(false)
  const syncingStock = ref(false)
  const orderPaymentChannels = ref<any[]>([])
  const orderPaymentChannelsRequestId = ref(0)
  let checkoutRevision = 0
  let disposed = false
  const hasCurrentToken = () => sessionToken === userAuthStore.token
    && sessionToken === (localStorage.getItem('user_token') || '')
  const isCurrentSession = () => !disposed && !reloading.value && route.name === 'checkout' && hasCurrentToken()

  // Payment state
  const selectedChannelId = ref<number | null>(null)
  const useBalance = ref(false)
  const walletLoading = ref(false)
  const walletBalance = ref('0')
  let walletBalanceRequestId = 0

  // Payment channels
  const paymentChannels = computed(() => {
    const list = userAuthStore.isAuthenticated
      ? orderPaymentChannels.value
      : appStore.config?.payment_channels
    if (!Array.isArray(list)) return []
    let filtered = list.filter((channel: any) => {
      const providerType = String(channel?.provider_type || '').toLowerCase()
      const channelType = String(channel?.channel_type || '').toLowerCase()
      if (providerType === 'epay') {
        return ['wechat', 'wxpay', 'alipay', 'qqpay'].includes(channelType)
      }
      return true
    })
    // 按购物车中商品允许的支付渠道交集过滤
    const items = cartItems.value
    if (items.length > 0) {
      let intersectionArr: number[] | null = null
      for (const item of items) {
        const ids = item.paymentChannelIds
        if (!Array.isArray(ids) || ids.length === 0) continue
        const idSet = new Set(ids.map(Number))
        if (intersectionArr === null) {
          intersectionArr = [...idSet]
        } else {
          intersectionArr = intersectionArr.filter((id) => idSet.has(id))
        }
      }
      if (intersectionArr !== null && intersectionArr.length > 0) {
        const allowedSet = new Set(intersectionArr)
        filtered = filtered.filter((ch: any) => allowedSet.has(Number(ch?.id)))
      } else if (intersectionArr !== null) {
        filtered = []
      }
    }
    return filtered.filter((ch: any) => !isChannelHiddenForAmount(ch))
  })

  const walletOnlyPayment = computed(() => !!appStore.config?.wallet_only_payment)
  // 分销白标店内后端会清零优惠券/促销/会员/批发等所有折扣（reseller 定价为固定加价），
  // 据此在结算页隐藏优惠券输入与恒为 0 的折扣预览行，避免买家填了无效优惠码。
  const isResellerTenant = computed(() => appStore.isResellerTenant)
  const showBalanceOption = computed(() => userAuthStore.isAuthenticated)
  const expectedWalletPaidCents = computed(() => {
    if (!showBalanceOption.value || !useBalance.value) return 0
    const balance = amountToCents(walletBalance.value)
    const total = amountToCents(previewTotal.value)
    if (balance === null || total === null) return 0
    return Math.min(balance, total)
  })
  const expectedOnlinePayCents = computed(() => {
    const total = amountToCents(previewTotal.value)
    if (total === null) return 0
    return Math.max(total - expectedWalletPaidCents.value, 0)
  })
  const expectedWalletPaidDisplay = computed(() => formatPrice(centsToAmount(expectedWalletPaidCents.value), previewCurrency.value))
  const expectedOnlinePayDisplay = computed(() => formatPrice(centsToAmount(expectedOnlinePayCents.value), previewCurrency.value))
  const requiresOnlineChannel = computed(() => {
    if (!userAuthStore.isAuthenticated) return true
    if (hasHostedRechargeItems.value) return true
    if (!useBalance.value) return true
    return expectedOnlinePayCents.value > 0
  })

  const channelLimitMeta = (channel?: any) => {
    const minCents = amountToCents(String(channel?.min_amount ?? ''))
    const maxCents = amountToCents(String(channel?.max_amount ?? ''))
    return {
      minCents,
      maxCents,
      hasMin: minCents !== null && minCents > 0,
      hasMax: maxCents !== null && maxCents > 0,
      hideAmountOutRange: Boolean(channel?.hide_amount_out_range),
    }
  }

  const isChannelHiddenForAmount = (channel?: any) => {
    if (!channel?.hide_amount_out_range) return false
    if (!requiresOnlineChannel.value) return false
    const targetAmount = expectedOnlinePayCents.value
    if (targetAmount <= 0) return false
    const meta = channelLimitMeta(channel)
    const lessThanMin = meta.hasMin && meta.minCents !== null && targetAmount < meta.minCents
    const greaterThanMax = meta.hasMax && meta.maxCents !== null && targetAmount > meta.maxCents
    return lessThanMin || greaterThanMax
  }

  const isChannelDisabledForAmount = (channel?: any) => {
    if (!requiresOnlineChannel.value) return false
    const targetAmount = expectedOnlinePayCents.value
    if (targetAmount <= 0) return false

    const meta = channelLimitMeta(channel)
    if (!meta.hasMin && !meta.hasMax) return false

    const lessThanMin = meta.hasMin && meta.minCents !== null && targetAmount < meta.minCents
    const greaterThanMax = meta.hasMax && meta.maxCents !== null && targetAmount > meta.maxCents
    if (!lessThanMin && !greaterThanMax) return false

    // 仅在“超出区间隐藏”未开启时，展示但置灰。
    return !meta.hideAmountOutRange
  }

  const channelAmountLimitHint = (channel?: any) => {
    const meta = channelLimitMeta(channel)
    if (meta.hasMin && meta.hasMax && meta.minCents !== null && meta.maxCents !== null) {
      return t('checkout.channelAmountLimitHint', {
        min: formatPrice(centsToAmount(meta.minCents), previewCurrency.value),
        max: formatPrice(centsToAmount(meta.maxCents), previewCurrency.value),
      })
    }
    if (meta.hasMin && meta.minCents !== null) {
      return t('checkout.channelAmountMinHint', {
        min: formatPrice(centsToAmount(meta.minCents), previewCurrency.value),
      })
    }
    if (meta.hasMax && meta.maxCents !== null) {
      return t('checkout.channelAmountMaxHint', {
        max: formatPrice(centsToAmount(meta.maxCents), previewCurrency.value),
      })
    }
    return ''
  }

  const handleSelectChannel = (channel?: any) => {
    if ((hasDirectItems.value && submitting.value) || !channel || isChannelDisabledForAmount(channel)) return
    selectedChannelId.value = Number(channel.id) || null
  }

  const selectedPaymentChannel = computed(() => selectedChannelId.value
    ? paymentChannels.value.find((channel: any) => Number(channel?.id) === selectedChannelId.value)
    : undefined)

  const selectedChannelAmountHint = computed(() => {
    const channel = selectedPaymentChannel.value
    if (!isChannelDisabledForAmount(channel)) return ''
    return channelAmountLimitHint(channel)
  })

  const formatChannelFeeRate = (channel?: any) => {
    const bp = rateToBasisPoints(channel?.fee_rate)
    if (bp === null) return '0.00%'
    return `${basisPointsToPercent(bp)}%`
  }

  const formatChannelFixedFee = (channel?: any) => {
    const fixed = channel?.fixed_fee
    if (fixed === null || fixed === undefined || fixed === '' || Number(fixed) === 0) {
      return formatPrice('0.00', previewCurrency.value)
    }
    return formatPrice(String(fixed), previewCurrency.value)
  }

  const totalAmount = computed(() => {
    const totalCents = cartItems.value.reduce((sum, item) => {
      const amountCents = amountToCents(item.priceAmount)
      const qty = parseInteger(item.quantity)
      if (amountCents === null || qty === null) return sum
      return sum + amountCents * qty
    }, 0)
    return centsToAmount(totalCents)
  })

  const totalCurrency = computed(() => siteCurrency.value || 'CNY')

  const previewCurrency = computed(() => preview.value?.currency || totalCurrency.value)
  const previewOriginal = computed(() => preview.value?.original_amount ?? totalAmount.value)
  const previewCoupon = computed(() => preview.value?.discount_amount ?? '0')
  const previewPromotion = computed(() => preview.value?.promotion_discount_amount ?? '0')
  const previewWholesale = computed(() => preview.value?.wholesale_discount_amount ?? '0')
  const previewMemberDiscount = computed(() => preview.value?.member_discount_amount ?? '0')
  const previewTotal = computed(() => preview.value?.total_amount ?? totalAmount.value)
  const checkoutItemCurrency = computed(() => previewCurrency.value)

  const previewItemsByKey = computed(() => {
    const map = new Map<string, any>()
    const items = Array.isArray(preview.value?.items) ? preview.value.items : []
    for (const item of items) {
      map.set(`${item.product_id}:${normalizeSkuId(item.sku_id)}`, item)
    }
    return map
  })

  const cartProductQuantities = computed(() => {
    const map = new Map<number, number>()
    for (const item of cartItems.value) {
      const productId = Number(item.productId || 0)
      const qty = parseInteger(item.quantity)
      if (!Number.isFinite(productId) || productId <= 0 || qty === null || qty <= 0) continue
      map.set(productId, (map.get(productId) || 0) + qty)
    }
    return map
  })

  const hasPositiveAmount = (amount: any) => {
    const cents = amountToCents(amount)
    return cents !== null && cents > 0
  }

  const formatDiscountPrice = (amount: any, currency?: any) => {
    return hasPositiveAmount(amount) ? `-${formatPrice(amount, currency)}` : formatPrice(amount, currency)
  }

  const checkoutMode = ref<'guest' | 'member'>('guest')
  const guestEmail = ref('')
  const guestPassword = ref('')
  const GUEST_PASSWORD_MIN_LENGTH = 6
  const guestPasswordValid = computed(() => guestPassword.value.trim().length >= GUEST_PASSWORD_MIN_LENGTH)
  const guestCaptchaPayload = ref<CaptchaPayload>({})
  const guestTurnstileToken = ref('')
  const guestImageCaptchaRef = ref<InstanceType<typeof ImageCaptcha> | null>(null)
  const guestTurnstileRef = ref<InstanceType<typeof TurnstileCaptcha> | null>(null)

  const manualFieldTypes = new Set(['text', 'textarea', 'phone', 'email', 'number', 'select', 'radio', 'checkbox'])
  const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
  const phonePattern = /^\+?[0-9\-()\s]{6,20}$/
  const findLastUnescapedSlash = (value: string) => {
    for (let index = value.length - 1; index > 0; index -= 1) {
      if (value[index] !== '/') {
        continue
      }
      let backslashes = 0
      for (let cursor = index - 1; cursor >= 0 && value[cursor] === '\\'; cursor -= 1) {
        backslashes += 1
      }
      if (backslashes % 2 === 0) {
        return index
      }
    }
    return -1
  }

  const compileManualRegex = (rawRegex?: string) => {
    const text = String(rawRegex || '').trim()
    if (!text) {
      return null
    }

    if (text.startsWith('/')) {
      const lastSlashIndex = findLastUnescapedSlash(text)
      if (lastSlashIndex > 0) {
        const pattern = text.slice(1, lastSlashIndex)
        const flags = text.slice(lastSlashIndex + 1)
        if (/^[gimsuy]*$/.test(flags)) {
          try {
            return new RegExp(pattern, flags)
          } catch {
            return null
          }
        }
      }
    }

    try {
      return new RegExp(text)
    } catch {
      return null
    }
  }

  const manualFormData = ref<Record<string, Record<string, any>>>({})
  const submitAttempted = ref(false)

  const normalizeManualFormSchema = (rawSchema: any): ManualFormField[] => {
    const rawFields = Array.isArray(rawSchema?.fields) ? rawSchema.fields : []
    return rawFields
      .map((rawField: any) => {
        const key = String(rawField?.key || '').trim()
        const type = String(rawField?.type || '').trim()
        if (!key || !manualFieldTypes.has(type)) {
          return null
        }
        const options = Array.isArray(rawField?.options)
          ? rawField.options.map((item: any) => String(item || '').trim()).filter(Boolean)
          : []
        const minValue = Number(rawField?.min)
        const maxValue = Number(rawField?.max)
        const maxLenValue = Number(rawField?.max_len)
        return {
          key,
          type,
          required: Boolean(rawField?.required),
          label: rawField?.label || undefined,
          placeholder: rawField?.placeholder || undefined,
          regex: String(rawField?.regex || '').trim() || undefined,
          min: Number.isFinite(minValue) ? minValue : undefined,
          max: Number.isFinite(maxValue) ? maxValue : undefined,
          max_len: Number.isFinite(maxLenValue) ? maxLenValue : undefined,
          options: Array.from(new Set(options)),
        } as ManualFormField
      })
      .filter(Boolean) as ManualFormField[]
  }

  const manualFormProducts = computed<ManualFormProduct[]>(() => {
    const grouped = new Map<number, ManualFormProduct>()
    cartItems.value.forEach((item) => {
      if (isSingleItemDirectType(item.fulfillmentType)) return
      // 按 schema 而非交付类型判断：对接商品对外展示为 auto 时也可能需要交付资料
      const fields = normalizeManualFormSchema(item.manualFormSchema)
      if (fields.length === 0) {
        return
      }
      const productId = Number(item.productId)
      if (!Number.isFinite(productId) || productId <= 0) {
        return
      }
      const normalizedProductId = Math.trunc(productId)
      const existing = grouped.get(normalizedProductId)
      if (existing) {
        existing.skuCount += 1
        return
      }
      grouped.set(normalizedProductId, {
        itemKey: String(normalizedProductId),
        productId: normalizedProductId,
        title: item.title,
        fields,
        skuCount: 1,
      })
    })
    return Array.from(grouped.values())
  })

  watch(manualFormProducts, (products) => {
    const nextData: Record<string, Record<string, any>> = {}
    products.forEach((product) => {
      const key = product.itemKey
      const current = manualFormData.value[key] || {}
      const formValues: Record<string, any> = {}
      product.fields.forEach((field) => {
        const currentValue = current[field.key]
        if (field.type === 'checkbox') {
          formValues[field.key] = Array.isArray(currentValue)
            ? currentValue.map((item: any) => String(item)).filter(Boolean)
            : []
        } else {
          formValues[field.key] = currentValue == null ? '' : String(currentValue)
        }
      })
      nextData[key] = formValues
    })
    manualFormData.value = nextData
  }, { immediate: true, deep: true })

  const resolveLocalizedText = (jsonData?: Record<string, string>, fallback = '') => {
    if (!jsonData) return fallback
    const locale = appStore.locale
    return jsonData[locale] || jsonData['zh-CN'] || jsonData['en-US'] || fallback
  }

  const getManualFieldLabel = (field: ManualFormField) => {
    return resolveLocalizedText(field.label, field.key)
  }

  const getManualFieldPlaceholder = (field: ManualFormField) => {
    return resolveLocalizedText(field.placeholder, '')
  }

  const manualFieldErrorKey = (itemKey: string, fieldKey: string) => `${itemKey}:${fieldKey}`

  const manualFormValidation = computed(() => {
    const errors: Record<string, string> = {}
    let firstError = ''

    const setError = (itemKey: string, field: ManualFormField, message: string) => {
      const errorKey = manualFieldErrorKey(itemKey, field.key)
      if (!errors[errorKey]) {
        errors[errorKey] = message
        if (!firstError) {
          firstError = message
        }
      }
    }

    manualFormProducts.value.forEach((product) => {
      const values = manualFormData.value[product.itemKey] || {}
      product.fields.forEach((field) => {
        const fieldLabel = getManualFieldLabel(field)
        const rawValue = values[field.key]
        if (field.type === 'checkbox') {
          const list = Array.isArray(rawValue)
            ? rawValue.map((item: any) => String(item).trim()).filter(Boolean)
            : []
          if (field.required && list.length === 0) {
            setError(product.itemKey, field, t('checkout.manualFormFieldRequired', { name: fieldLabel }))
            return
          }
          if (list.length > 0 && field.options.length > 0 && list.some((item) => !field.options.includes(item))) {
            setError(product.itemKey, field, t('checkout.manualFormFieldOptionInvalid', { name: fieldLabel }))
          }
          return
        }

        const text = rawValue == null ? '' : String(rawValue).trim()
        if (field.required && !text) {
          setError(product.itemKey, field, t('checkout.manualFormFieldRequired', { name: fieldLabel }))
          return
        }
        if (!text) {
          return
        }

        if ((field.type === 'text' || field.type === 'textarea' || field.type === 'phone' || field.type === 'email') && field.max_len && text.length > field.max_len) {
          setError(product.itemKey, field, t('checkout.manualFormFieldMaxLength', { name: fieldLabel, max: field.max_len }))
          return
        }
        if ((field.type === 'phone' && !phonePattern.test(text)) || (field.type === 'email' && !emailPattern.test(text))) {
          setError(product.itemKey, field, t('checkout.manualFormFieldInvalid', { name: fieldLabel }))
          return
        }
        if (field.type === 'number') {
          const numberValue = Number(text)
          if (!Number.isFinite(numberValue)) {
            setError(product.itemKey, field, t('checkout.manualFormFieldNumberInvalid', { name: fieldLabel }))
            return
          }
          if ((field.min !== undefined && numberValue < field.min) || (field.max !== undefined && numberValue > field.max)) {
            setError(product.itemKey, field, t('checkout.manualFormFieldNumberRange', { name: fieldLabel }))
            return
          }
        }
        if ((field.type === 'select' || field.type === 'radio') && field.options.length > 0 && !field.options.includes(text)) {
          setError(product.itemKey, field, t('checkout.manualFormFieldOptionInvalid', { name: fieldLabel }))
          return
        }
        if (field.regex) {
          const regex = compileManualRegex(field.regex)
          if (!regex || !regex.test(text)) {
            setError(product.itemKey, field, t('checkout.manualFormFieldInvalid', { name: fieldLabel }))
          }
        }
      })
    })

    return {
      valid: Object.keys(errors).length === 0,
      errors,
      firstError,
    }
  })

  const manualFieldError = (itemKey: string, fieldKey: string) => {
    return manualFormValidation.value.errors[manualFieldErrorKey(itemKey, fieldKey)] || ''
  }

  const buildManualFormDataPayload = () => {
    const payload: Record<string, any> = {}
    manualFormProducts.value.forEach((product) => {
      const values = manualFormData.value[product.itemKey] || {}
      const row: Record<string, any> = {}
      product.fields.forEach((field) => {
        const rawValue = values[field.key]
        if (field.type === 'checkbox') {
          const list = Array.isArray(rawValue)
            ? rawValue.map((item: any) => String(item).trim()).filter(Boolean)
            : []
          if (list.length > 0) {
            row[field.key] = list
          }
          return
        }
        const text = rawValue == null ? '' : String(rawValue).trim()
        if (text) {
          row[field.key] = text
        }
      })
      payload[product.itemKey] = row
    })
    return payload
  }

  const manualFormFingerprint = computed(() => JSON.stringify(manualFormData.value))

  const canGuestCheckout = computed(() => (
    !hasDirectItems.value || (!directCheckoutError.value && cartItems.value.every((item) => item.purchaseType === 'guest'))
  ))
  const isGuestCheckout = computed(() => canGuestCheckout.value && !userAuthStore.isAuthenticated && checkoutMode.value === 'guest')
  const guestEmailValid = computed(() => {
    if (!isGuestCheckout.value) return true
    return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(guestEmail.value.trim())
  })

  const captchaConfig = computed(() => appStore.config?.captcha || null)
  const captchaProvider = computed(() => String(captchaConfig.value?.provider || 'none'))
  const guestCaptchaEnabled = computed(() => {
    if (!isGuestCheckout.value) return false
    return !!captchaConfig.value?.scenes?.guest_create_order && captchaProvider.value !== 'none'
  })
  const guestTurnstileSiteKey = computed(() => String(captchaConfig.value?.turnstile?.site_key || ''))

  const getGuestCaptchaPayload = (): CaptchaPayload | undefined => {
    if (!guestCaptchaEnabled.value) return undefined
    if (captchaProvider.value === 'image') {
      return {
        captcha_id: guestCaptchaPayload.value.captcha_id || '',
        captcha_code: guestCaptchaPayload.value.captcha_code || '',
      }
    }
    if (captchaProvider.value === 'turnstile') {
      return {
        turnstile_token: guestTurnstileToken.value,
      }
    }
    return undefined
  }

  const handleGuestCaptchaConfigStale = async () => {
    await appStore.loadConfig(true)
    guestCaptchaPayload.value = {}
    guestTurnstileToken.value = ''
  }

  const submitBlockedReason = computed(() => {
    if (directCheckoutError.value) return t(directCheckoutError.value)
    if (syncingStock.value) return t('checkout.stockSyncing')
    if (cartItems.value.length === 0) return t('checkout.errors.emptyCart')
    if (!manualFormValidation.value.valid) {
      return manualFormValidation.value.firstError || t('checkout.errors.manualFormInvalid')
    }
    const stockBlockedItem = cartItems.value.find((item) => itemStockExceeded(item))
    if (stockBlockedItem) {
      return itemStockHint(stockBlockedItem) || t('cart.stockOut')
    }
    const minBlockedItem = cartItems.value.find((item) => itemMinNotMet(item))
    if (minBlockedItem) {
      return t('cart.minPurchaseNotMet', { count: itemPurchaseMin(minBlockedItem) })
    }
    if (walletOnlyPayment.value && hasHostedRechargeItems.value) return t('hostedRecharge.walletOnlyUnavailable')
    if (walletOnlyPayment.value && expectedOnlinePayCents.value > 0) return t('payment.walletInsufficientHint')
    const selectedChannel = hasDirectItems.value ? selectedPaymentChannel.value : selectedChannelId.value
    if (!walletOnlyPayment.value && requiresOnlineChannel.value && !selectedChannel) return t('checkout.errors.selectPayment')
    if (requiresOnlineChannel.value && selectedChannelAmountHint.value) return selectedChannelAmountHint.value
    if (userAuthStore.isAuthenticated) return ''
    if (checkoutMode.value !== 'guest') return t('checkout.errors.loginOrGuest')
    if (!guestEmail.value.trim() || !guestPassword.value.trim()) return t('checkout.errors.missingGuest')
    if (!guestEmailValid.value) return t('error.email_invalid')
    if (!guestPasswordValid.value) return t('checkout.errors.guestPasswordTooShort')
    if (guestCaptchaEnabled.value) {
      if ((captchaProvider.value === 'image' && (!guestCaptchaPayload.value.captcha_id || !guestCaptchaPayload.value.captcha_code))
        || (captchaProvider.value === 'turnstile' && !guestTurnstileToken.value)) {
        return t('auth.common.captchaRequired')
      }
    }
    return ''
  })

  const canSubmit = computed(() => !reloading.value && !submitting.value && !submitBlockedReason.value)

  const previewStatusText = computed(() => couponRefreshing.value
    ? t('checkout.couponRefreshing')
    : t('checkout.previewLoading'))

  const checkoutAlert = computed<PageAlert | null>(() => {
    if (error.value) {
      return { level: 'error' as const, message: error.value }
    }
    if (previewError.value) {
      return { level: 'error' as const, message: previewError.value }
    }
    if (!canSubmit.value && submitBlockedReason.value) {
      return { level: 'warning' as const, message: submitBlockedReason.value }
    }
    return null
  })

  const buildItemsPayload = () => cartItems.value.map(item => ({
    product_id: item.productId,
    sku_id: normalizeSkuId(item.skuId) || undefined,
    quantity: item.quantity,
    fulfillment_type: isDirectFulfillmentType(item.fulfillmentType)
      ? GPT_DIRECT
      : isHostedRechargeType(item.fulfillmentType)
        ? HOSTED_RECHARGE
        : item.fulfillmentType || undefined,
  }))

  const buildOrderPayload = () => ({
    coupon_code: normalizedCouponCode.value || undefined,
    affiliate_code: getAffiliateCode() || undefined,
    affiliate_visitor_key: getAffiliateVisitorKey() || undefined,
    items: buildItemsPayload(),
    manual_form_data: buildManualFormDataPayload(),
  })

  const isCurrentCheckout = (revision: number) => revision === checkoutRevision && isCurrentSession()

  const isValidAmount = (amount: unknown) => {
    if (typeof amount !== 'string' && typeof amount !== 'number') return false
    const cents = amountToCents(amount)
    return cents !== null && Number(amount) >= 0
  }

  const loadOrderPaymentChannels = async (items = buildItemsPayload()) => {
    if (!isCurrentSession()) return false
    const requestId = ++orderPaymentChannelsRequestId.value
    if (!userAuthStore.isAuthenticated || directCheckoutError.value || !requiresOnlineChannel.value
      || items.length === 0 || !preview.value) {
      orderPaymentChannels.value = []
      return false
    }
    const revision = checkoutRevision
    const requestedPreview = preview.value
    const onlineAmount = expectedOnlinePayCents.value > 0
      ? expectedOnlinePayCents.value
      : (hasHostedRechargeItems.value ? (amountToCents(previewTotal.value) ?? 0) : 0)
    const isCurrentRequest = () => requestId === orderPaymentChannelsRequestId.value && isCurrentCheckout(revision)
      && requestedPreview === preview.value && requiresOnlineChannel.value
      && onlineAmount === (expectedOnlinePayCents.value > 0
        ? expectedOnlinePayCents.value
        : (hasHostedRechargeItems.value ? (amountToCents(previewTotal.value) ?? 0) : 0))
    try {
      const response = await userOrderAPI.getPaymentChannels({
        amount: centsToAmount(onlineAmount),
        items,
      })
      if (!isCurrentRequest()) return false
      const channels = response.data.data
      const available = Array.isArray(channels)
      orderPaymentChannels.value = available ? channels : []
      return available
    } catch {
      if (isCurrentRequest()) {
        const fallback = requestedPreview.payment_channels
        orderPaymentChannels.value = Array.isArray(fallback) ? fallback : []
      }
      return false
    }
  }

  const debouncedLoadOrderPaymentChannels = debounceAsync(loadOrderPaymentChannels, 250)

  const syncCartStockSnapshots = async () => {
    if (isBuyNowMode.value) return
    if (syncingStock.value) return
    syncingStock.value = true
    try {
      await refreshCartStockSnapshots(cartStore)
    } finally {
      syncingStock.value = false
    }
  }

  const loadPreview = async (orderPayload?: ReturnType<typeof buildOrderPayload>) => {
    if (!isCurrentSession()) return false
    const requestId = ++previewRequestId.value
    directPreviewRetryable.value = false
    if (directCheckoutError.value || syncingStock.value || cartItems.value.length === 0
      || (isGuestCheckout.value && (!guestEmail.value.trim() || !guestPassword.value.trim() || !guestEmailValid.value))
      || cartItems.value.some((item) => itemStockExceeded(item) || itemMinNotMet(item))) {
      orderPaymentChannelsRequestId.value++
      preview.value = null
      previewLoading.value = false
      orderPaymentChannels.value = []
      previewError.value = ''
      couponRefreshing.value = false
      return false
    }

    const revision = checkoutRevision
    const isCurrentRequest = () => requestId === previewRequestId.value && isCurrentCheckout(revision)
    previewLoading.value = true
    previewError.value = ''

    try {
      const payload = orderPayload ?? buildOrderPayload()

      let response
      if (userAuthStore.isAuthenticated) {
        response = await userOrderAPI.preview(payload)
      } else {
        response = await guestOrderAPI.preview({
          ...payload,
          email: guestEmail.value.trim(),
          order_password: guestPassword.value,
        })
      }

      if (!isCurrentRequest()) return false
      const result = response.data.data
      if (hasDirectItems.value && !isValidAmount(result?.total_amount)) {
        throw new Error(t('checkout.previewFailed'))
      }
      preview.value = result
      return true
    } catch (err: any) {
      if (isCurrentRequest()) {
        preview.value = null
        orderPaymentChannels.value = []
        previewError.value = err.message || t('checkout.previewFailed')
        if (hasDirectItems.value && err instanceof ApiRequestError) {
          const status = err.status ?? 0
          directPreviewRetryable.value = err.kind === 'network' || err.kind === 'timeout'
            || status === 408 || status === 429 || (status >= 500 && status < 600)
        }
      }
      return false
    } finally {
      if (requestId === previewRequestId.value && isCurrentSession()) {
        previewLoading.value = false
        couponRefreshing.value = false
      }
    }
  }

  const debouncedLoadPreview = debounceAsync(loadPreview, 300)

  const canRetryDirectPreview = computed(() => hasDirectItems.value && directPreviewRetryable.value
    && !directCheckoutError.value && !previewLoading.value && !submitting.value && !reloading.value)
  const retryDirectPreview = () => {
    if (!canRetryDirectPreview.value || !isCurrentSession()) return
    debouncedLoadPreview.cancel()
    debouncedLoadOrderPaymentChannels.cancel()
    error.value = ''
    return loadPreview()
  }

  const clearSourceStore = () => {
    if (isBuyNowMode.value) {
      buyNowStore.clear()
    } else {
      cartStore.clear()
    }
  }

  const handleSubmit = async () => {
    checkStoredSession()
    if (!isCurrentSession() || submitting.value) return
    submitAttempted.value = true
    error.value = ''
    previewError.value = ''
    if (!canSubmit.value) {
      error.value = submitBlockedReason.value || t('checkout.errors.submitFailed')
      return
    }

    submitting.value = true
    const revision = checkoutRevision
    const buyNow = isBuyNowMode.value
    const directSubmission = hasDirectItems.value
    const useWallet = useBalance.value
    const walletOnly = walletOnlyPayment.value
    const isCurrentPaymentPreference = () => useWallet === useBalance.value && walletOnly === walletOnlyPayment.value
    debouncedLoadPreview.cancel()
    debouncedLoadOrderPaymentChannels.cancel()
    orderPaymentChannelsRequestId.value++
    try {
      const orderPayload = buildOrderPayload()
      const hasPreview = await loadPreview(orderPayload)
      if (!isCurrentCheckout(revision)) return
      if (previewError.value || (directSubmission && (!hasPreview || !isCurrentPaymentPreference()))) {
        error.value = previewError.value || t('checkout.previewFailed')
        return
      }
      const authenticated = userAuthStore.isAuthenticated
      if (directSubmission) {
        if (useWallet) {
          const hasBalance = await loadWalletBalance()
          if (!isCurrentCheckout(revision)) return
          if (!hasBalance || !isCurrentPaymentPreference()) {
            error.value = t('checkout.errors.submitFailed')
            return
          }
        }
        if (walletOnly && expectedOnlinePayCents.value > 0) {
          error.value = t('payment.walletInsufficientHint')
          return
        }
        if (authenticated && requiresOnlineChannel.value) {
          const hasChannels = await loadOrderPaymentChannels(orderPayload.items)
          if (!isCurrentCheckout(revision)) return
          if (!hasChannels || !isCurrentPaymentPreference()) {
            error.value = t('checkout.errors.submitFailed')
            return
          }
        }
        if (submitBlockedReason.value) {
          error.value = submitBlockedReason.value
          return
        }
      }

      const payload = {
        ...orderPayload,
        channel_id: requiresOnlineChannel.value ? (selectedChannelId.value || undefined) : undefined,
        use_balance: directSubmission ? useWallet : useBalance.value,
      }

      const guestAuth = authenticated ? null : {
        email: guestEmail.value.trim(),
        order_password: guestPassword.value,
      }
      const response = guestAuth
        ? await guestOrderAPI.createAndPay({
          ...payload,
          ...guestAuth,
          captcha_payload: getGuestCaptchaPayload(),
        })
        : await userOrderAPI.createAndPay(payload, sessionToken)
      if (!isCurrentSession()) return

      const responseData = response.data.data
      if (!responseData?.order_no) {
        throw new Error(t('checkout.errors.submitFailed'))
      }

      if (guestAuth) saveGuestOrderAuth(guestAuth)
      // A successful order still opens payment if the same buyer edited the form during the request.
      // Preserve any newly selected items, and clear only an unchanged purchase selection.
      if (buyNow === isBuyNowMode.value && JSON.stringify(buildItemsPayload()) === JSON.stringify(orderPayload.items)) {
        clearSourceStore()
      }

      // Redirect to the existing Payment page which handles all payment display
      const query = authenticated
        ? `order_no=${encodeURIComponent(responseData.order_no)}`
        : `guest=1&order_no=${encodeURIComponent(responseData.order_no)}`
      if (isCurrentSession()) void router.push(`/pay?${query}`)
    } catch (err: any) {
      if (!isCurrentCheckout(revision)) return
      error.value = err.message || t('checkout.errors.submitFailed')
      if (guestCaptchaEnabled.value && captchaProvider.value === 'image') {
        guestImageCaptchaRef.value?.refresh()
      }
      if (guestCaptchaEnabled.value && captchaProvider.value === 'turnstile') {
        guestTurnstileRef.value?.reset()
        guestTurnstileToken.value = ''
      }
    } finally {
      if (isCurrentSession()) {
        submitting.value = false
        if (!isCurrentCheckout(revision)) {
          debouncedLoadPreview()
        } else if (directSubmission && !isCurrentPaymentPreference()) {
          debouncedLoadOrderPaymentChannels()
        }
      }
    }
  }

  watch(
    () => [cartItems.value, manualFormFingerprint.value, normalizedCouponCode.value, checkoutMode.value,
      guestEmail.value, guestPassword.value, userAuthStore.isAuthenticated, userAuthStore.token,
      directCheckoutError.value, route.fullPath],
    () => {
      checkoutRevision++
      directPreviewRetryable.value = false
      if (isCurrentSession() && !submitting.value) debouncedLoadPreview()
    },
    { deep: true, flush: 'sync' }
  )

  watch(walletOnlyPayment, (v) => {
    if (v) useBalance.value = true
  }, { immediate: true })

  watch(normalizedCouponCode, (value, previous) => {
    if (value === previous) return
    couponRefreshing.value = true
    error.value = ''
    previewError.value = ''
  })

  watch(
    () => [userAuthStore.isAuthenticated, requiresOnlineChannel.value, expectedOnlinePayCents.value, preview.value],
    () => {
      if (isCurrentSession() && (!hasDirectItems.value || !submitting.value)) debouncedLoadOrderPaymentChannels()
    }
  )

  watch(
    () => [selectedPaymentChannel.value, expectedOnlinePayCents.value, requiresOnlineChannel.value, submitting.value],
    () => {
      if ((hasDirectItems.value && submitting.value) || !selectedChannelId.value) return
      const selected = selectedPaymentChannel.value
      if (!selected || isChannelDisabledForAmount(selected)) {
        selectedChannelId.value = null
      }
    },
    { deep: true }
  )

  const loadWalletBalance = async () => {
    if (!isCurrentSession() || !userAuthStore.isAuthenticated) return false
    const requestId = ++walletBalanceRequestId
    const isCurrentRequest = () => isCurrentSession() && requestId === walletBalanceRequestId
    walletLoading.value = true
    try {
      const response = await walletAPI.account()
      if (!isCurrentRequest()) return false
      const balance = response.data.data?.balance
      if (hasDirectItems.value && !isValidAmount(balance)) return false
      walletBalance.value = String(balance || '0')
      return true
    } catch {
      if (isCurrentRequest() && !hasDirectItems.value) walletBalance.value = '0'
      return false
    } finally {
      if (isCurrentRequest()) walletLoading.value = false
    }
  }

  const checkStoredSession = () => {
    if (disposed || reloading.value || route.name !== 'checkout' || hasCurrentToken()) return
    reloading.value = true
    debouncedLoadPreview.cancel()
    debouncedLoadOrderPaymentChannels.cancel()
    preview.value = null
    previewLoading.value = false
    previewError.value = ''
    directPreviewRetryable.value = false
    couponCode.value = ''
    couponRefreshing.value = false
    orderPaymentChannels.value = []
    selectedChannelId.value = null
    walletBalance.value = '0'
    walletLoading.value = false
    manualFormData.value = {}
    guestEmail.value = ''
    guestPassword.value = ''
    guestCaptchaPayload.value = {}
    guestTurnstileToken.value = ''
    submitting.value = false
    error.value = ''
    window.location.reload()
  }
  const handleStoredSessionChange = (event: StorageEvent) => {
    if (event.storageArea === localStorage && (event.key === 'user_token' || event.key === null)) checkStoredSession()
  }

  onMounted(async () => {
    window.addEventListener('storage', handleStoredSessionChange)
    window.addEventListener('pageshow', checkStoredSession)
    document.addEventListener('visibilitychange', checkStoredSession)
    checkStoredSession()
    if (!isCurrentSession()) return
    if (!appStore.config) {
      await appStore.loadConfig()
    }
    if (!isCurrentSession()) return
    await syncCartStockSnapshots()
    if (!isCurrentSession() || submitting.value) return
    debouncedLoadPreview()
    loadWalletBalance()
  })

  onUnmounted(() => {
    disposed = true
    debouncedLoadPreview.cancel()
    debouncedLoadOrderPaymentChannels.cancel()
    window.removeEventListener('storage', handleStoredSessionChange)
    window.removeEventListener('pageshow', checkStoredSession)
    document.removeEventListener('visibilitychange', checkStoredSession)
  })

  const cartItemKey = (item: CartItem) => `${item.productId}:${normalizeSkuId(item.skuId)}`

  const itemSkuDisplay = (item: CartItem) => {
    if (isDirectFulfillmentType(item.fulfillmentType) && item.skuSpecValues?.plan && item.skuSpecValues?.country) {
      return t('directPurchase.sku', { plan: item.skuSpecValues.plan, country: item.skuSpecValues.country })
    }
    return buildSkuDisplayText({
      skuCode: item.skuCode,
      specValues: item.skuSpecValues,
      fallback: t('productDetail.skuFallback'),
      locale: appStore.locale,
    })
  }

  const checkoutItemImage = (item: CartItem) => {
    const rawImage = String(item.image || '').trim()
    if (!rawImage) return ''
    return getImageUrl(rawImage)
  }

  const cartItemSubtotalCents = (item: CartItem) => {
    const amountCents = amountToCents(item.priceAmount)
    const qty = parseInteger(item.quantity)
    if (amountCents === null || qty === null) {
      return null
    }
    return amountCents * qty
  }

  const cartItemWholesaleSubtotalCents = (item: CartItem) => {
    const qty = parseInteger(item.quantity)
    if (qty === null || qty <= 0 || !Array.isArray(item.wholesalePrices)) {
      return null
    }
    const productId = Number(item.productId || 0)
    const matchQuantity = cartProductQuantities.value.get(productId) || qty
    const matchedPriceCents = amountToCents(resolveWholesalePriceAmount(
      item,
      item.priceAmount,
      matchQuantity,
      item.skuId,
      item.skuCode,
      qty,
    ))
    if (matchedPriceCents === null) return null
    return matchedPriceCents * qty
  }

  const checkoutItemPreview = (item: CartItem) => previewItemsByKey.value.get(cartItemKey(item))

  const checkoutItemOriginalCents = (item: CartItem) => {
    const previewItem = checkoutItemPreview(item)
    const previewOriginalCents = amountToCents(previewItem?.original_total_price)
    if (previewOriginalCents !== null) return previewOriginalCents
    return cartItemSubtotalCents(item)
  }

  const checkoutItemPayableCents = (item: CartItem) => {
    const previewItem = checkoutItemPreview(item)
    const previewPayableCents = amountToCents(previewItem?.total_price)
    if (previewPayableCents !== null) {
      const couponDiscountCents = amountToCents(previewItem?.coupon_discount_amount) || 0
      return Math.max(0, previewPayableCents - couponDiscountCents)
    }
    const wholesaleSubtotal = cartItemWholesaleSubtotalCents(item)
    if (wholesaleSubtotal !== null) return wholesaleSubtotal
    return checkoutItemOriginalCents(item)
  }

  const pricePartsFromCents = (cents: number | null) => {
    if (cents === null) return { integer: '-', decimal: '' }
    const amount = centsToAmount(Math.max(0, cents))
    const [integer, decimal = ''] = amount.split('.')
    return {
      integer,
      decimal: decimal ? `.${decimal}` : '',
    }
  }

  const checkoutItemPriceParts = (item: CartItem) => pricePartsFromCents(checkoutItemPayableCents(item))

  const checkoutItemOriginalPriceParts = (item: CartItem) => pricePartsFromCents(checkoutItemOriginalCents(item))

  const checkoutItemHasPriceDiscount = (item: CartItem) => {
    const originalCents = checkoutItemOriginalCents(item)
    const payableCents = checkoutItemPayableCents(item)
    return originalCents !== null && payableCents !== null && originalCents > payableCents
  }

  const normalizeStockNumber = (value: unknown) => {
    const numberValue = Number(value)
    if (!Number.isFinite(numberValue)) return 0
    return Math.max(Math.floor(numberValue), 0)
  }

  const normalizeManualStockTotal = (value: unknown) => {
    const numberValue = Number(value)
    if (!Number.isFinite(numberValue)) return 0
    const integerValue = Math.floor(numberValue)
    if (integerValue === -1) return -1
    return Math.max(integerValue, 0)
  }

  const hasItemStockSnapshot = (item: CartItem) => Boolean(String(item.skuStockSnapshotAt || '').trim())

  const shouldEnforceItemStock = (item: CartItem) => {
    if (item.skuStockQuantityHidden === true) return false
    if (item.fulfillmentType === 'auto') return true
    if (item.fulfillmentType === 'upstream') return true
    if (item.fulfillmentType !== 'manual') return false
    if (!hasItemStockSnapshot(item)) return false
    const total = normalizeManualStockTotal(item.skuManualStockTotal)
    if (total === -1) return false
    if (item.skuStockEnforced === true) return true
    if (item.skuStockEnforced === false) return false
    return true
  }

  const itemAvailableStock = (item: CartItem) => {
    if (item.skuStockQuantityHidden === true) {
      return item.skuStockStatus === 'out_of_stock' ? 0 : null
    }
    if (!shouldEnforceItemStock(item)) return null
    if (item.fulfillmentType === 'upstream') {
      const upstreamStock = Number(item.skuUpstreamStock ?? 0)
      if (upstreamStock === -1) return null // 无限库存
      return Math.max(upstreamStock, 0)
    }
    if (item.fulfillmentType === 'auto') {
      return normalizeStockNumber(item.skuAutoStockAvailable)
    }
    const total = normalizeManualStockTotal(item.skuManualStockTotal)
    if (total === -1) return null
    return total
  }

  const itemMaxQuantity = (item: CartItem) => {
    const available = itemAvailableStock(item)
    const purchaseLimit = itemPurchaseLimit(item)
    if (available === null && purchaseLimit === null) return Number.MAX_SAFE_INTEGER
    if (available === null) return purchaseLimit || 0
    if (purchaseLimit === null) return Math.max(available, 0)
    return Math.max(Math.min(available, purchaseLimit), 0)
  }

  const itemStockExceeded = (item: CartItem) => {
    const qty = parseInteger(item.quantity)
    if (qty === null) return true
    return qty > itemMaxQuantity(item)
  }

  const itemMinNotMet = (item: CartItem) => {
    const qty = parseInteger(item.quantity)
    if (qty === null) return false
    return qty < itemPurchaseMin(item)
  }

  const itemStockHint = (item: CartItem) => {
    const available = itemAvailableStock(item)
    const purchaseLimit = itemPurchaseLimit(item)
    const maxQuantity = itemMaxQuantity(item)
    if (itemMinNotMet(item)) {
      return t('cart.minPurchaseNotMet', { count: itemPurchaseMin(item) })
    }
    if (available === null && purchaseLimit === null) return ''
    if (maxQuantity <= 0) return t('cart.stockOut')
    if (itemStockExceeded(item)) {
      if (purchaseLimit !== null && maxQuantity === purchaseLimit && (available === null || purchaseLimit < available)) {
        return t('cart.maxPurchaseExceeded', { count: purchaseLimit })
      }
      return t('cart.stockExceeded', { count: maxQuantity })
    }
    if (available === null) return ''
    if (available <= 0) return t('cart.stockOut')
    return t('cart.stockRemaining', { count: available })
  }

  return {
    // stores / helpers
    userAuthStore,
    getLocalizedText,
    formatPrice,
    getImageUrl,
    // mode / items
    isBuyNowMode,
    hasDirectItems,
    checkoutLoginTo,
    cartItems,
    totalItems,
    cartItemKey,
    checkoutItemImage,
    itemSkuDisplay,
    itemStockExceeded,
    itemStockHint,
    checkoutItemCurrency,
    checkoutItemPriceParts,
    checkoutItemOriginalPriceParts,
    checkoutItemHasPriceDiscount,
    // manual form
    manualFormProducts,
    manualFormData,
    submitAttempted,
    getManualFieldLabel,
    getManualFieldPlaceholder,
    manualFieldError,
    // coupon
    couponCode,
    isResellerTenant,
    // mode select / guest
    checkoutMode,
    canGuestCheckout,
    isGuestCheckout,
    guestEmail,
    guestPassword,
    guestEmailValid,
    guestCaptchaEnabled,
    captchaProvider,
    guestCaptchaPayload,
    guestTurnstileToken,
    guestTurnstileSiteKey,
    guestImageCaptchaRef,
    guestTurnstileRef,
    handleGuestCaptchaConfigStale,
    // preview amounts
    previewCurrency,
    previewOriginal,
    previewCoupon,
    previewPromotion,
    previewWholesale,
    previewMemberDiscount,
    previewTotal,
    previewLoading,
    couponRefreshing,
    previewStatusText,
    canRetryDirectPreview,
    retryDirectPreview,
    hasPositiveAmount,
    formatDiscountPrice,
    checkoutAlert,
    // wallet / balance
    showBalanceOption,
    walletLoading,
    walletBalance,
    useBalance,
    walletOnlyPayment,
    expectedWalletPaidDisplay,
    expectedOnlinePayDisplay,
    expectedOnlinePayCents,
    // payment channels
    requiresOnlineChannel,
    paymentChannels,
    selectedChannelId,
    isChannelDisabledForAmount,
    channelAmountLimitHint,
    handleSelectChannel,
    formatChannelFeeRate,
    formatChannelFixedFee,
    // submit
    submitting,
    canSubmit,
    handleSubmit,
  }
}
