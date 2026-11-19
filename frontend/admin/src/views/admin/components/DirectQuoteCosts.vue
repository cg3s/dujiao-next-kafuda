<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { AdminDirectQuoteCosts } from '@/api/types'

defineProps<{ costs: AdminDirectQuoteCosts }>()

const { t } = useI18n()
const entries = [
  { field: 'service_fee_usd', label: 'serviceFee' },
  { field: 'card_open_fee_usd', label: 'cardOpenFee' },
  { field: 'card_load_usd', label: 'cardLoad' },
  { field: 'total_usd', label: 'total' },
] as const
</script>

<template>
  <div class="mt-4 border-t border-border pt-3">
    <h4 class="mb-2 text-sm font-medium">{{ t('admin.orders.directReview.quoteCosts.title') }}</h4>
    <dl class="grid gap-3 text-sm sm:grid-cols-2">
      <div v-for="{ field, label } in entries" :key="field" class="min-w-0">
        <dt class="text-muted-foreground">{{ t(`admin.orders.directReview.quoteCosts.${label}`) }}</dt>
        <dd class="break-all font-mono">{{ costs[field] }} USD</dd>
      </div>
    </dl>
  </div>
</template>
