<template>
  <section class="rounded-xl border bg-card p-5" aria-live="polite">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex flex-wrap items-center gap-2.5">
        <h2 class="text-lg font-semibold">{{ t('hostedRecharge.progress.title') }}</h2>
        <Badge v-if="status" :variant="stageBadgeVariant" size="sm" class="rounded-full">
          {{ t(`hostedRecharge.progress.${status.stage}`) }}
        </Badge>
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        class="rounded-full"
        :disabled="opening"
        @click="emit('refresh')"
      >
        <RefreshCw class="h-3.5 w-3.5" />
        {{ t('hostedRecharge.progress.refresh') }}
      </Button>
    </div>

    <div v-if="status" class="mt-3 space-y-2">
      <p class="text-sm text-foreground">{{ t(`hostedRecharge.stageHints.${status.stage}`) }}</p>
      <p v-if="status.progress_message" class="rounded-lg bg-secondary/70 px-3 py-2 text-xs text-muted-foreground">
        {{ status.progress_message }}
      </p>
      <p class="text-xs text-muted-foreground">
        {{ t('hostedRecharge.progress.updated', { time: formatDate(status.progress_updated_at || status.updated_at) }) }}
      </p>
    </div>

    <p v-if="unavailable" class="mt-3 text-sm text-muted-foreground">
      {{ t('hostedRecharge.progress.retrying') }}
    </p>
    <p v-else-if="!status" class="mt-3 text-sm text-muted-foreground">
      {{ t('common.loading') }}
    </p>

    <div v-if="status?.can_open_fulfillment && !unavailable" class="mt-4 flex flex-wrap items-center gap-3">
      <Button
        type="button"
        class="rounded-full font-semibold"
        :disabled="opening"
        @click="handleOpenEntry"
      >
        <ExternalLink class="h-4 w-4" />
        {{
          opening
            ? t('hostedRecharge.entry.opening')
            : t(status.stage === 'resubmit_required' ? 'hostedRecharge.entry.resubmitButton' : 'hostedRecharge.entry.openButton')
        }}
      </Button>
      <span class="text-xs text-muted-foreground">{{ t('hostedRecharge.entry.hint') }}</span>
    </div>

    <div
      v-if="fallbackUrl"
      class="mt-3 flex flex-wrap items-center gap-2 rounded-lg border bg-secondary/50 px-3 py-2 text-xs text-muted-foreground"
    >
      <span>{{ t('hostedRecharge.entry.popupBlocked') }}</span>
      <a
        :href="fallbackUrl"
        target="_blank"
        rel="noopener noreferrer"
        class="font-semibold text-primary underline underline-offset-2"
      >
        {{ t('hostedRecharge.entry.openManually') }}
      </a>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ExternalLink, RefreshCw } from 'lucide-vue-next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { guestOrderAPI, userOrderAPI } from '../api'
import { ApiRequestError } from '../api/client'
import { toast } from '../composables/useToast'
import type { HostedRechargeStatus } from '../utils/hostedRechargeStatus'

const props = defineProps<{
  orderNo: string
  status: HostedRechargeStatus | null
  unavailable: boolean
  formatDate: (value: string) => string
  guestAuth?: { email: string; order_password: string }
}>()

const emit = defineEmits<{
  (e: 'refresh'): void
}>()

const { t } = useI18n()
const opening = ref(false)
const fallbackUrl = ref('')

watch(() => props.orderNo, () => {
  fallbackUrl.value = ''
})

watch(() => props.status?.can_open_fulfillment, (canOpen) => {
  if (!canOpen) fallbackUrl.value = ''
})

const stageBadgeVariant = computed<'success' | 'warning' | 'destructive' | 'info' | 'neutral'>(() => {
  switch (props.status?.stage) {
    case 'succeeded':
      return 'success'
    case 'resubmit_required':
    case 'procurement_blocked':
    case 'manual_review':
      return 'warning'
    case 'closed':
    case 'refunded':
      return 'neutral'
    case 'refunding':
      return 'destructive'
    default:
      return 'info'
  }
})

const handleOpenEntry = async () => {
  if (opening.value || !props.orderNo) return
  opening.value = true
  fallbackUrl.value = ''
  try {
    const response = props.guestAuth
      ? await guestOrderAPI.openHostedRechargeEntry(props.orderNo, props.guestAuth)
      : await userOrderAPI.openHostedRechargeEntry(props.orderNo)
    const url = String(response.data?.data?.fulfillment_url || '').trim()
    if (!url || !/^https?:\/\//i.test(url)) {
      toast.error(t('hostedRecharge.entry.failed'))
      emit('refresh')
      return
    }
    const popup = window.open(url, '_blank', 'noopener,noreferrer')
    if (!popup) {
      fallbackUrl.value = url
    }
    emit('refresh')
  } catch (error) {
    toast.error(error instanceof ApiRequestError ? error.message : t('hostedRecharge.entry.failed'))
    emit('refresh')
  } finally {
    opening.value = false
  }
}
</script>
