import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { userOrderAPI } from '../api'
import { ApiRequestError } from '../api/client'
import { debounceAsync } from '../utils/debounce'
import { directStatusOrderNo, startDirectOrderPolling, type DirectOrderStatus } from '../utils/directOrderStatus'
import { useConfirmDialog } from './useConfirmDialog'
import { toast } from './useToast'
import { useOrderDisplayHelpers } from './useOrderDisplayHelpers'

const ORDER_LOAD_RETRY_DELAYS = [2_000, 5_000]
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
  const directStatusUnavailable = ref(false)
  const directOrderNo = computed(() => order.value?.order_no === orderNo.value ? directStatusOrderNo(order.value) : '')

  const helpers = useOrderDisplayHelpers(order)
  const sessionToken = localStorage.getItem('user_token')
  let orderRequest = 0
  let reloading = false
  let disposed = false
  let inFlight: Promise<void> | undefined
  let retryTimer: ReturnType<typeof setTimeout> | undefined
  let retries = 0
  let initialAttempted = false
  let retryableFailure = false
  const isCurrentSession = () => !disposed && !reloading && localStorage.getItem('user_token') === sessionToken

  const clearInitialRetry = () => {
    clearTimeout(retryTimer)
    retryTimer = undefined
  }

  const resetOrderLoad = () => {
    orderRequest++
    inFlight = undefined
    clearInitialRetry()
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

  const handleOrderConnectivity = () => {
    const reconnected = orderOffline.value && navigator.onLine
    orderOffline.value = !navigator.onLine
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

  watch(directOrderNo, (number, _, onCleanup) => {
    directStatus.value = null
    directStatusUnavailable.value = false
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
      if (isCurrent()) await debouncedLoadOrder()
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
    debouncedLoadOrder,
    cancelOrder,
    fulfillmentDownloading,
    handleDownloadFulfillment,
    ...helpers,
  }
}
