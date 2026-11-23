import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { userOrderAPI } from '../api'
import { ApiRequestError } from '../api/client'
import { debounceAsync } from '../utils/debounce'
import { directPendingPaymentOrderNo, directStatusOrderNo, startDirectOrderPolling, type DirectOrderStatus } from '../utils/directOrderStatus'
import {
  hostedRechargeStatusOrderNo,
  startHostedRechargePolling,
  type HostedRechargeStatus,
} from '../utils/hostedRechargeStatus'
import { useConfirmDialog } from './useConfirmDialog'
import { toast } from './useToast'
import { useOrderDisplayHelpers } from './useOrderDisplayHelpers'

const ORDER_LOAD_RETRY_DELAYS = [2_000, 5_000]
const DIRECT_PAYMENT_REFRESH_INTERVAL = 10_000
type OrderLoadMode = 'foreground' | 'retry' | 'sync'

/**
 * 已登录用户订单详情逻辑（classic + vault 共用）。
 */
export function useOrderDetail() {
  const route = useRoute()
  const router = useRouter()
  const { confirm: showConfirm } = useConfirmDialog()
  const { t } = useI18n()

  const loading = ref(true)
  const order = ref<any>(null)
  const orderOffline = ref(!navigator.onLine)
  const orderLoadFailed = ref(false)
  const fulfillmentDownloading = ref(false)
  const orderNo = computed(() => String(route.params.order_no || '').trim())
  const directStatus = ref<DirectOrderStatus | null>(null)
  const directStatusRefresh = ref(0)
  const directStatusUnavailable = ref(false)
  const hostedRechargeStatus = ref<HostedRechargeStatus | null>(null)
  const hostedRechargeRefresh = ref(0)
  const hostedRechargeUnavailable = ref(false)
  const currentOrder = computed(() => order.value?.order_no === orderNo.value ? order.value : null)
  const directOrderNo = computed(() => directStatusOrderNo(currentOrder.value))
  const hostedRechargeOrderNo = computed(() => hostedRechargeStatusOrderNo(currentOrder.value))
  const pendingDirectOrderNo = computed(() => directPendingPaymentOrderNo(currentOrder.value))

  const helpers = useOrderDisplayHelpers(order)
  const sessionToken = localStorage.getItem('user_token')
  let orderRequest = 0
  let reloading = false
  let disposed = false
  let inFlight: Promise<void> | undefined
  let retryTimer: ReturnType<typeof setTimeout> | undefined
  let directPaymentTimer: ReturnType<typeof setTimeout> | undefined
  let retries = 0
  let initialAttempted = false
  let retryableFailure = false
  const isCurrentSession = () => !disposed && !reloading && localStorage.getItem('user_token') === sessionToken

  const clearInitialRetry = () => {
    clearTimeout(retryTimer)
    retryTimer = undefined
  }

  const clearDirectPaymentRefresh = () => {
    clearTimeout(directPaymentTimer)
    directPaymentTimer = undefined
  }

  const resetOrderLoad = () => {
    orderRequest++
    inFlight = undefined
    clearInitialRetry()
    clearDirectPaymentRefresh()
    debouncedLoadOrder.cancel()
    retries = 0
    initialAttempted = false
    retryableFailure = false
    orderLoadFailed.value = false
  }

  const checkStoredSession = () => {
    if (reloading || isCurrentSession()) return
    reloading = true
    resetOrderLoad()
    order.value = null
    directStatus.value = null
    directStatusUnavailable.value = false
    hostedRechargeStatus.value = null
    hostedRechargeUnavailable.value = false
    loading.value = true
    // 重新经过路由鉴权，并丢弃旧账号的页面状态。
    window.location.reload()
  }
  const handleStoredSessionChange = (event: StorageEvent) => {
    if (event.storageArea === localStorage && (event.key === 'user_token' || event.key === null)) {
      checkStoredSession()
    }
  }

  const handleDownloadFulfillment = async (number: string) => {
    if (fulfillmentDownloading.value || !isCurrentSession()) return
    fulfillmentDownloading.value = true
    try {
      const res = await userOrderAPI.downloadFulfillment(number)
      if (!isCurrentSession()) return
      const blob = new Blob([res.data], { type: 'text/plain; charset=utf-8' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `fulfillment-${number}.txt`
      a.click()
      URL.revokeObjectURL(url)
    } catch {} finally {
      fulfillmentDownloading.value = false
    }
  }

  const canLoadInitialOrder = () => isCurrentSession() && !!orderNo.value && !order.value && !inFlight
    && document.visibilityState === 'visible' && navigator.onLine

  const canRetryInitialOrder = () => canLoadInitialOrder() && retryableFailure && retries < ORDER_LOAD_RETRY_DELAYS.length

  const retryInitialOrder = (): void => {
    clearInitialRetry()
    if (!canRetryInitialOrder()) return
    retries++
    void loadOrder('retry')
  }

  const scheduleInitialRetry = (): void => {
    if (retryTimer !== undefined || !canRetryInitialOrder()) return
    retryTimer = setTimeout(retryInitialOrder, ORDER_LOAD_RETRY_DELAYS[retries])
  }

  const loadOrder = async (mode: OrderLoadMode = 'foreground'): Promise<void> => {
    if (!isCurrentSession() || !orderNo.value) return
    if (inFlight) return inFlight
    const number = orderNo.value
    orderOffline.value = !navigator.onLine
    if (orderOffline.value) {
      loading.value = false
      return
    }
    const request = ++orderRequest
    const isCurrent = () => isCurrentSession() && request === orderRequest && number === orderNo.value
    clearInitialRetry()
    initialAttempted = true
    if (mode === 'foreground') loading.value = true
    inFlight = (async () => {
      try {
        const response = await userOrderAPI.detail(number, { silentBusinessError: mode !== 'foreground' })
        if (!isCurrent()) return
        if (response.data.data?.order_no !== number) {
          if (mode !== 'sync') {
            order.value = null
            orderLoadFailed.value = true
            retryableFailure = false
          }
          return
        }
        order.value = response.data.data
        orderLoadFailed.value = false
        retryableFailure = false
        retries = 0
      } catch (error) {
        if (mode === 'sync' || !isCurrent()) return
        order.value = null
        orderOffline.value = !navigator.onLine
        const failure = error instanceof ApiRequestError ? error : undefined
        const status = failure?.status ?? 0
        orderLoadFailed.value = status !== 404 && status !== 403
        retryableFailure = failure?.kind === 'network' || failure?.kind === 'timeout'
          || status === 408 || (status >= 500 && status < 600)
      }
    })().finally(() => {
      if (!isCurrent()) return
      inFlight = undefined
      loading.value = false
      scheduleInitialRetry()
    })
    return inFlight
  }

  const canRefreshDirectPayment = (number: string) => !!number && pendingDirectOrderNo.value === number
    && isCurrentSession() && document.visibilityState === 'visible' && navigator.onLine

  const scheduleDirectPaymentRefresh = (number: string) => {
    clearDirectPaymentRefresh()
    if (canRefreshDirectPayment(number)) {
      directPaymentTimer = setTimeout(() => void refreshDirectPayment(number), DIRECT_PAYMENT_REFRESH_INTERVAL)
    }
  }

  const refreshDirectPayment = async (number: string) => {
    if (pendingDirectOrderNo.value !== number) return
    clearDirectPaymentRefresh()
    if (!canRefreshDirectPayment(number)) return
    await loadOrder('sync')
    if (canRefreshDirectPayment(number)) scheduleDirectPaymentRefresh(number)
  }

  const handleOrderConnectivity = () => {
    const reconnected = orderOffline.value && navigator.onLine
    orderOffline.value = !navigator.onLine
    if (pendingDirectOrderNo.value) {
      void refreshDirectPayment(pendingDirectOrderNo.value)
      return
    }
    if (!canLoadInitialOrder()) {
      clearInitialRetry()
      return
    }
    if (!initialAttempted) void loadOrder()
    else if (reconnected) retryInitialOrder()
    else scheduleInitialRetry()
  }

  const handlePageResume = () => {
    checkStoredSession()
    handleOrderConnectivity()
  }

  const debouncedLoadOrder = debounceAsync(() => {
    retries = 0
    return loadOrder()
  }, 300)

  watch(pendingDirectOrderNo, scheduleDirectPaymentRefresh)

  watch([directOrderNo, directStatusRefresh], ([number], [previousNumber], onCleanup) => {
    if (number !== previousNumber) {
      directStatus.value = null
      directStatusUnavailable.value = false
    }
    if (!number) return
    onCleanup(startDirectOrderPolling(
      async () => {
        if (!isCurrentSession()) return null
        const response = await userOrderAPI.directStatus(number)
        return response.data.data
      },
      (status) => {
        if (!isCurrentSession() || number !== directOrderNo.value) return
        directStatusUnavailable.value = status === null
        if (!status) return
        directStatus.value = status
        if (status.stage === 'manual_review' && order.value?.status === 'paid') void loadOrder('sync')
      },
      document,
      async (stage) => {
        if (!isCurrentSession() || number !== directOrderNo.value || order.value?.status === stage) return true
        await loadOrder('sync')
        return !isCurrentSession() || number !== directOrderNo.value || order.value?.status === stage
      },
      { isOnline: () => navigator.onLine, events: window },
    ))
  })

  watch([hostedRechargeOrderNo, hostedRechargeRefresh], ([number], [previousNumber], onCleanup) => {
    if (number !== previousNumber) {
      hostedRechargeStatus.value = null
      hostedRechargeUnavailable.value = false
    }
    if (!number) return
    onCleanup(startHostedRechargePolling(
      async () => {
        if (!isCurrentSession()) return null
        const response = await userOrderAPI.hostedRechargeStatus(number)
        return response.data.data
      },
      (status) => {
        if (!isCurrentSession() || number !== hostedRechargeOrderNo.value) return
        hostedRechargeUnavailable.value = status === null
        if (!status) return
        hostedRechargeStatus.value = status
      },
      document,
      async (stage) => {
        const expected = stage === 'succeeded' ? 'completed' : stage === 'refunded' ? 'refunded' : ''
        if (!isCurrentSession() || number !== hostedRechargeOrderNo.value) return true
        if (expected && order.value?.status === expected) return true
        await loadOrder('sync')
        if (!expected) return true
        return !isCurrentSession() || number !== hostedRechargeOrderNo.value || order.value?.status === expected
      },
      { isOnline: () => navigator.onLine, events: window },
    ))
  })

  watch(orderNo, (number) => {
    resetOrderLoad()
    order.value = null
    if (number) void loadOrder()
  }, { flush: 'sync' })

  const cancelOrder = async () => {
    const number = order.value?.order_no
    const isCurrent = () => isCurrentSession() && number === orderNo.value
    if (!number || !isCurrent()) return
    const confirmed = await showConfirm({
      title: t('orderDetail.cancel'),
      message: t('orderDetail.cancelConfirm'),
      confirmText: t('common.confirm'),
      cancelText: t('common.cancel'),
      variant: 'danger',
    })
    if (!confirmed || !isCurrent()) return
    try {
      await userOrderAPI.cancel(number)
      if (!isCurrent()) return
      if (pendingDirectOrderNo.value === number) {
        // A payment refresh started before cancellation may still carry the old status.
        resetOrderLoad()
        await refreshDirectPayment(number)
      } else {
        await debouncedLoadOrder()
      }
    } catch {
      if (isCurrent()) toast.error(t('orderDetail.cancelFailed'))
    }
  }

  onMounted(() => {
    window.addEventListener('storage', handleStoredSessionChange)
    window.addEventListener('online', handleOrderConnectivity)
    window.addEventListener('offline', handleOrderConnectivity)
    window.addEventListener('pageshow', handlePageResume)
    document.addEventListener('visibilitychange', handlePageResume)
    checkStoredSession()
    if (!isCurrentSession()) return
    if (!route.params.order_no) {
      router.push('/me/orders')
      return
    }
    loadOrder()
  })

  onUnmounted(() => {
    disposed = true
    window.removeEventListener('storage', handleStoredSessionChange)
    window.removeEventListener('online', handleOrderConnectivity)
    window.removeEventListener('offline', handleOrderConnectivity)
    window.removeEventListener('pageshow', handlePageResume)
    document.removeEventListener('visibilitychange', handlePageResume)
    resetOrderLoad()
  })

  return {
    loading,
    order,
    orderOffline,
    orderLoadFailed,
    directOrderNo,
    directStatus,
    directStatusUnavailable,
    refreshDirectStatus: () => { directStatusRefresh.value++ },
    hostedRechargeOrderNo,
    hostedRechargeStatus,
    hostedRechargeUnavailable,
    refreshHostedRechargeStatus: () => { hostedRechargeRefresh.value++ },
    debouncedLoadOrder,
    cancelOrder,
    fulfillmentDownloading,
    handleDownloadFulfillment,
    ...helpers,
  }
}
