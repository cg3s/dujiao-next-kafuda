import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { userOrderAPI } from '../api'
import { debounceAsync } from '../utils/debounce'
import { directStatusOrderNo, startDirectOrderPolling, type DirectOrderStatus } from '../utils/directOrderStatus'
import { useConfirmDialog } from './useConfirmDialog'
import { toast } from './useToast'
import { useOrderDisplayHelpers } from './useOrderDisplayHelpers'

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
  const fulfillmentDownloading = ref(false)
  const orderNo = computed(() => String(route.params.order_no || '').trim())
  const directStatus = ref<DirectOrderStatus | null>(null)
  const directStatusUnavailable = ref(false)
  const directOrderNo = computed(() => order.value?.order_no === orderNo.value ? directStatusOrderNo(order.value) : '')

  const helpers = useOrderDisplayHelpers(order)
  const sessionToken = localStorage.getItem('user_token')
  let orderRequest = 0
  let reloading = false
  const isCurrentSession = () => !reloading && localStorage.getItem('user_token') === sessionToken

  const checkStoredSession = () => {
    if (reloading || isCurrentSession()) return
    reloading = true
    orderRequest++
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

  const loadOrder = async (showLoading = true) => {
    if (!isCurrentSession()) return
    const request = ++orderRequest
    const number = orderNo.value
    const isCurrent = () => isCurrentSession() && request === orderRequest && number === orderNo.value
    if (order.value?.order_no !== number) order.value = null
    orderOffline.value = !navigator.onLine
    if (orderOffline.value) {
      loading.value = false
      return
    }
    if (showLoading) loading.value = true
    try {
      const response = await userOrderAPI.detail(number, { silentBusinessError: !showLoading })
      if (isCurrent()) order.value = response.data.data
    } catch {
      if (showLoading && isCurrent()) order.value = null
    } finally {
      if (isCurrent()) loading.value = false
    }
  }

  const handleOrderConnectivity = () => {
    const offline = !navigator.onLine
    if (orderOffline.value === offline) return
    orderOffline.value = offline
    if (orderNo.value && order.value?.order_no !== orderNo.value) void loadOrder()
  }

  const handlePageResume = () => {
    checkStoredSession()
    handleOrderConnectivity()
  }

  const debouncedLoadOrder = debounceAsync(loadOrder, 300)

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
        if (status.stage === 'manual_review' && order.value?.status === 'paid') void loadOrder(false)
      },
      document,
      async (stage) => {
        if (!isCurrentSession() || number !== directOrderNo.value || order.value?.status === stage) return true
        await loadOrder(false)
        return !isCurrentSession() || number !== directOrderNo.value || order.value?.status === stage
      },
      { isOnline: () => navigator.onLine, events: window },
    ))
  })

  watch(orderNo, (number) => {
    if (number) void loadOrder()
  })

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
    window.removeEventListener('storage', handleStoredSessionChange)
    window.removeEventListener('online', handleOrderConnectivity)
    window.removeEventListener('offline', handleOrderConnectivity)
    window.removeEventListener('pageshow', handlePageResume)
    document.removeEventListener('visibilitychange', handlePageResume)
    orderRequest++
    debouncedLoadOrder.cancel()
  })

  return {
    loading,
    order,
    orderOffline,
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
