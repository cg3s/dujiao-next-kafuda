import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { guestOrderAPI } from '../api'
import { debounceAsync } from '../utils/debounce'
import { directStatusOrderNo, startDirectOrderPolling, type DirectOrderStatus } from '../utils/directOrderStatus'
import {
  hostedRechargeStatusOrderNo,
  startHostedRechargePolling,
  type HostedRechargeStatus,
} from '../utils/hostedRechargeStatus'
import { clearGuestOrderAuth, loadGuestOrderAuth, saveGuestOrderAuth } from '../utils/guestOrderAuth'
import { resolveGuestOrderDetailViewState } from '../utils/guestOrderDetailState'
import { useOrderDisplayHelpers } from './useOrderDisplayHelpers'

/**
 * 游客订单详情逻辑（classic + vault 共用）。
 */
export function useGuestOrderDetail() {
  const route = useRoute()
  const router = useRouter()
  const { t } = useI18n()

  const loading = ref(true)
  const order = ref<any>(null)
  const authError = ref('')
  const auth = ref({
    email: '',
    order_password: '',
  })
  const fulfillmentDownloading = ref(false)
  const directStatus = ref<DirectOrderStatus | null>(null)
  const directStatusRefresh = ref(0)
  const directStatusUnavailable = ref(false)
  const hostedRechargeStatus = ref<HostedRechargeStatus | null>(null)
  const hostedRechargeRefresh = ref(0)
  const hostedRechargeUnavailable = ref(false)
  const orderNo = computed(() => String(route.params.order_no || '').trim())
  const currentOrder = computed(() => order.value?.order_no === orderNo.value ? order.value : null)
  const directOrderNo = computed(() => directStatusOrderNo(currentOrder.value))
  const hostedRechargeOrderNo = computed(() => hostedRechargeStatusOrderNo(currentOrder.value))

  const helpers = useOrderDisplayHelpers(order)

  const handleDownloadFulfillment = async (orderNo: string) => {
    if (fulfillmentDownloading.value) return
    fulfillmentDownloading.value = true
    try {
      const res = await guestOrderAPI.downloadFulfillment(orderNo, {
        email: auth.value.email,
        order_password: auth.value.order_password,
      })
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

  const loadSavedAuth = () => {
    auth.value = loadGuestOrderAuth()
  }

  const hasAuth = computed(() => Boolean(auth.value.email && auth.value.order_password))
  const showAuthForm = computed(() => !hasAuth.value || authError.value !== '')
  const viewState = computed(() => resolveGuestOrderDetailViewState({
    loading: loading.value,
    order: order.value,
    showAuthForm: showAuthForm.value,
  }))

  const loadOrder = async () => {
    loading.value = true
    try {
      if (!hasAuth.value) {
        order.value = null
        authError.value = t('guestOrderDetail.authRequired')
        return
      }
      const response = await guestOrderAPI.detail(String(route.params.order_no || '').trim(), {
        email: auth.value.email,
        order_password: auth.value.order_password,
      })
      order.value = response.data.data
      authError.value = ''
    } catch (error) {
      order.value = null
      authError.value = t('guestOrderDetail.authInvalid')
    } finally {
      loading.value = false
    }
  }

  const debouncedLoadOrder = debounceAsync(loadOrder, 300)

  const persistAuth = () => {
    saveGuestOrderAuth({
      email: auth.value.email,
      order_password: auth.value.order_password,
    })
  }

  const handleAuthSubmit = async () => {
    authError.value = ''
    if (!hasAuth.value) {
      authError.value = t('guestOrderDetail.authRequired')
      return
    }
    persistAuth()
    loading.value = true
    await debouncedLoadOrder()
  }

  const clearAuth = () => {
    clearGuestOrderAuth()
    auth.value = { email: '', order_password: '' }
    order.value = null
    directStatus.value = null
    directStatusUnavailable.value = false
    hostedRechargeStatus.value = null
    hostedRechargeUnavailable.value = false
    authError.value = t('guestOrderDetail.authRequired')
  }

  let disposed = false

  watch([directOrderNo, directStatusRefresh], ([number], _prev, onCleanup) => {
    directStatus.value = null
    directStatusUnavailable.value = false
    if (!number || !hasAuth.value) return
    const credentials = {
      email: auth.value.email,
      order_password: auth.value.order_password,
    }
    onCleanup(startDirectOrderPolling(
      async () => {
        const response = await guestOrderAPI.directStatus(number, credentials)
        return response.data.data
      },
      (status) => {
        if (disposed || number !== directOrderNo.value) return
        directStatusUnavailable.value = status === null
        if (!status) return
        directStatus.value = status
        if (status.stage === 'manual_review' && order.value?.status === 'paid') void loadOrder()
      },
      document,
      async (stage) => {
        if (disposed || number !== directOrderNo.value || order.value?.status === stage) return true
        await loadOrder()
        return disposed || number !== directOrderNo.value || order.value?.status === stage
      },
      { isOnline: () => navigator.onLine, events: window },
    ))
  })

  watch([hostedRechargeOrderNo, hostedRechargeRefresh], ([number], _prev, onCleanup) => {
    hostedRechargeStatus.value = null
    hostedRechargeUnavailable.value = false
    if (!number || !hasAuth.value) return
    const credentials = {
      email: auth.value.email,
      order_password: auth.value.order_password,
    }
    onCleanup(startHostedRechargePolling(
      async () => {
        const response = await guestOrderAPI.hostedRechargeStatus(number, credentials)
        return response.data.data
      },
      (status) => {
        if (disposed || number !== hostedRechargeOrderNo.value) return
        hostedRechargeUnavailable.value = status === null
        if (!status) return
        hostedRechargeStatus.value = status
      },
      document,
      async (stage) => {
        const expected = stage === 'succeeded' ? 'completed' : stage === 'refunded' ? 'refunded' : ''
        if (disposed || number !== hostedRechargeOrderNo.value) return true
        if (expected && order.value?.status === expected) return true
        await loadOrder()
        if (!expected) return true
        return disposed || number !== hostedRechargeOrderNo.value || order.value?.status === expected
      },
      { isOnline: () => navigator.onLine, events: window },
    ))
  })

  onMounted(() => {
    if (!route.params.order_no) {
      router.push('/guest/orders')
      return
    }
    loadSavedAuth()
    loadOrder()
  })

  onUnmounted(() => {
    disposed = true
    debouncedLoadOrder.cancel()
  })

  return {
    loading,
    order,
    directOrderNo,
    directStatus,
    directStatusUnavailable,
    refreshDirectStatus: () => { directStatusRefresh.value++ },
    hostedRechargeOrderNo,
    hostedRechargeStatus,
    hostedRechargeUnavailable,
    refreshHostedRechargeStatus: () => { hostedRechargeRefresh.value++ },
    authError,
    auth,
    showAuthForm,
    viewState,
    handleAuthSubmit,
    clearAuth,
    fulfillmentDownloading,
    handleDownloadFulfillment,
    ...helpers,
  }
}
