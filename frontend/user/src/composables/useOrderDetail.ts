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
  const fulfillmentDownloading = ref(false)
  const orderNo = computed(() => String(route.params.order_no || '').trim())
  const directStatus = ref<DirectOrderStatus | null>(null)
  const directStatusUnavailable = ref(false)
  const directOrderNo = computed(() => order.value?.order_no === orderNo.value ? directStatusOrderNo(order.value) : '')

  const helpers = useOrderDisplayHelpers(order)

  const handleDownloadFulfillment = async (orderNo: string) => {
    if (fulfillmentDownloading.value) return
    fulfillmentDownloading.value = true
    try {
      const res = await userOrderAPI.downloadFulfillment(orderNo)
      const blob = new Blob([res.data], { type: 'text/plain; charset=utf-8' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `fulfillment-${orderNo}.txt`
      a.click()
      URL.revokeObjectURL(url)
    } catch {} finally {
      fulfillmentDownloading.value = false
    }
  }

  let orderRequest = 0
  const loadOrder = async (showLoading = true) => {
    const request = ++orderRequest
    const number = orderNo.value
    if (showLoading) loading.value = true
    try {
      const response = await userOrderAPI.detail(number, { silentBusinessError: !showLoading })
      if (request === orderRequest && number === orderNo.value) order.value = response.data.data
    } catch {
      if (showLoading && request === orderRequest && number === orderNo.value) order.value = null
    } finally {
      if (request === orderRequest) loading.value = false
    }
  }

  const debouncedLoadOrder = debounceAsync(loadOrder, 300)

  watch(directOrderNo, (number, _, onCleanup) => {
    directStatus.value = null
    directStatusUnavailable.value = false
    if (!number) return
    onCleanup(startDirectOrderPolling(
      async () => {
        const response = await userOrderAPI.directStatus(number)
        return response.data.data
      },
      (status) => {
        if (number !== directOrderNo.value) return
        directStatusUnavailable.value = status === null
        if (!status) return
        directStatus.value = status
        if (status.stage === 'manual_review' && order.value?.status === 'paid') void loadOrder(false)
      },
      document,
      async (stage) => {
        if (number !== directOrderNo.value || order.value?.status === stage) return true
        await loadOrder(false)
        return number !== directOrderNo.value || order.value?.status === stage
      },
    ))
  })

  watch(orderNo, (number) => {
    if (number) void loadOrder()
  })

  const cancelOrder = async () => {
    const number = order.value?.order_no
    if (!number || number !== orderNo.value) return
    const confirmed = await showConfirm({
      title: t('orderDetail.cancel'),
      message: t('orderDetail.cancelConfirm'),
      confirmText: t('common.confirm'),
      cancelText: t('common.cancel'),
      variant: 'danger',
    })
    if (!confirmed || number !== orderNo.value) return
    try {
      await userOrderAPI.cancel(number)
      if (number === orderNo.value) await debouncedLoadOrder()
    } catch {
      toast.error(t('orderDetail.cancelFailed'))
    }
  }

  onMounted(() => {
    if (!route.params.order_no) {
      router.push('/me/orders')
      return
    }
    loadOrder()
  })

  onUnmounted(() => {
    orderRequest++
    debouncedLoadOrder.cancel()
  })

  return {
    loading,
    order,
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
