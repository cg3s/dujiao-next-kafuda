<template>
  <section class="rounded-xl border bg-card p-5" aria-live="polite">
    <h2 class="text-lg font-semibold">{{ t('directPurchase.progress.title') }}</h2>
    <p v-if="status" class="mt-2 text-sm font-medium">{{ t(`directPurchase.progress.${status.stage}`) }}</p>
    <p v-if="status" class="mt-1 text-xs text-muted-foreground">
      {{ t('directPurchase.progress.updated', { time: formatDate(status.updated_at) }) }}
    </p>
    <p v-if="unavailable" class="mt-2 text-sm text-muted-foreground">{{ t('directPurchase.progress.retrying') }}</p>
    <p v-else-if="!status" class="mt-2 text-sm text-muted-foreground">{{ t('common.loading') }}</p>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { DirectOrderStatus } from '../utils/directOrderStatus'

defineProps<{
  status: DirectOrderStatus | null
  unavailable: boolean
  formatDate: (value: string) => string
}>()

const { t } = useI18n()
</script>
