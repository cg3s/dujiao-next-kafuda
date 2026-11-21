<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  adminAPI,
  type KafudaCardProduct,
  type KafudaConfigData,
  type KafudaConfigUpdatePayload,
} from '@/api/admin'
import { useAdminAuthStore } from '@/stores/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Badge } from '@/components/ui/badge'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { notifyError, notifySuccess } from '@/utils/notify'
import { formatDate } from '@/utils/format'
import { ChevronDown, ChevronRight, Eye, EyeOff, RefreshCw, ShieldAlert, CheckCircle2 } from 'lucide-vue-next'

const { t, te } = useI18n()
const authStore = useAdminAuthStore()

const loading = ref(false)
const saving = ref(false)
const testing = ref(false)
const syncing = ref(false)
const showApiKey = ref(false)
const showAdvanced = ref(false)

const status = reactive({
  config_source: 'config_file',
  version: 0,
  updated_at: '',
  has_api_key: false,
  configured: false,
  gateway_config_invalid: false,
  worker_ready: false,
  sales_block_reason: '',
  catalog_enabled: false,
  catalog_stale: true,
  catalog_synced_at: '',
  catalog_expires_at: '',
  catalog_options_count: 0,
})

const cards = ref<KafudaCardProduct[]>([])

const form = reactive({
  api_base_url: 'https://kafuda.com',
  api_key: '',
  clear_api_key: false,
  product_code: '',
  max_cost_cents: 0,
  allowed_kinds_text: 'new',
  sales_enabled: false,
  reference_lookup_verified: false,
  catalog_ttl_seconds: 3600,
  catalog_refresh_interval_seconds: 0,
  poll_review_after_seconds: 86400,
  poll_interval_seconds: 10,
})

const canUpdate = computed(() => authStore.hasPermission('PUT:/admin/kafuda-gpt/config'))
const canTest = computed(() => authStore.hasPermission('POST:/admin/kafuda-gpt/config/test'))
const canSync = computed(() => authStore.hasPermission('POST:/admin/kafuda-gpt/catalog/sync'))

const maxCostUSD = computed(() => {
  const cents = Number(form.max_cost_cents)
  if (Number.isNaN(cents) || cents <= 0) return '0.00'
  return (cents / 100).toFixed(2)
})

const sourceBadgeText = computed(() => {
  if (status.config_source === 'database') return t('kafuda.config.sourceDatabase')
  if (status.config_source === 'error') return t('kafuda.config.sourceError')
  return t('kafuda.config.sourceConfigFile')
})

const salesBlockMessage = computed(() => {
  if (!status.sales_block_reason) return ''
  const key = `kafuda.salesBlockReasons.${status.sales_block_reason}`
  return te(key) ? t(key) : status.sales_block_reason
})

const formatCentsUSD = (cents?: number) => {
  if (cents === undefined || cents === null || Number.isNaN(Number(cents))) return '-'
  return `$${(Number(cents) / 100).toFixed(2)}`
}

const parseAllowedKinds = (raw: string): string[] => {
  const seen = new Set<string>()
  const result: string[] = []
  for (const part of raw.split(',')) {
    const item = part.trim().toLowerCase()
    if (!item || seen.has(item)) continue
    seen.add(item)
    result.push(item)
  }
  return result
}

const applyConfigData = (data: KafudaConfigData) => {
  status.config_source = data.config_source || 'config_file'
  status.version = Number(data.version || 0)
  status.updated_at = data.updated_at || ''
  status.has_api_key = Boolean(data.has_api_key)
  status.configured = Boolean(data.configured)
  status.gateway_config_invalid = Boolean(data.gateway_config_invalid)
  status.worker_ready = Boolean(data.worker_ready)
  status.sales_block_reason = data.sales_block_reason || ''
  status.catalog_enabled = Boolean(data.catalog_enabled)
  status.catalog_stale = Boolean(data.catalog_stale)
  status.catalog_synced_at = data.catalog_synced_at || ''
  status.catalog_expires_at = data.catalog_expires_at || ''
  status.catalog_options_count = Number(data.catalog_options_count || 0)
  cards.value = Array.isArray(data.cards) ? data.cards : []

  form.api_base_url = data.api_base_url || 'https://kafuda.com'
  form.api_key = ''
  form.clear_api_key = false
  form.product_code = data.product_code || ''
  form.max_cost_cents = Number(data.max_cost_cents || 0)
  form.allowed_kinds_text = Array.isArray(data.allowed_kinds) && data.allowed_kinds.length > 0
    ? data.allowed_kinds.join(', ')
    : ''
  form.sales_enabled = Boolean(data.sales_enabled)
  form.reference_lookup_verified = Boolean(data.reference_lookup_verified)
  form.catalog_ttl_seconds = Number(data.catalog_ttl_seconds || 3600)
  form.catalog_refresh_interval_seconds = Number(data.catalog_refresh_interval_seconds ?? 0)
  form.poll_review_after_seconds = Number(data.poll_review_after_seconds || 86400)
  form.poll_interval_seconds = Number(data.poll_interval_seconds || 10)
}

const loadConfig = async () => {
  loading.value = true
  try {
    const res = await adminAPI.getKafudaConfig()
    const data = res.data?.data as KafudaConfigData | undefined
    if (data) {
      applyConfigData(data)
    }
  } catch (err) {
    const known = err as Error & { __notified?: boolean }
    if (!known?.__notified) {
      notifyError(known?.message || t('kafuda.config.alerts.loadFailed'))
    }
  } finally {
    loading.value = false
  }
}

const buildPayload = (): KafudaConfigUpdatePayload => {
  const payload: KafudaConfigUpdatePayload = {
    api_base_url: form.api_base_url.trim(),
    product_code: form.product_code.trim(),
    max_cost_cents: Math.max(0, Math.trunc(Number(form.max_cost_cents) || 0)),
    allowed_kinds: parseAllowedKinds(form.allowed_kinds_text),
    sales_enabled: form.sales_enabled,
    reference_lookup_verified: form.reference_lookup_verified,
    catalog_ttl_seconds: Math.trunc(Number(form.catalog_ttl_seconds) || 3600),
    catalog_refresh_interval_seconds: Math.trunc(Number(form.catalog_refresh_interval_seconds) || 0),
    poll_review_after_seconds: Math.trunc(Number(form.poll_review_after_seconds) || 86400),
    poll_interval_seconds: Math.trunc(Number(form.poll_interval_seconds) || 10),
  }
  if (form.clear_api_key) {
    payload.clear_api_key = true
  } else if (form.api_key.trim()) {
    payload.api_key = form.api_key.trim()
  }
  return payload
}

const handleSave = async () => {
  if (!canUpdate.value) return
  saving.value = true
  try {
    const res = await adminAPI.updateKafudaConfig(buildPayload())
    const data = res.data?.data as KafudaConfigData | undefined
    if (data) {
      applyConfigData(data)
    }
    notifySuccess(t('kafuda.config.alerts.saveSuccess'))
  } catch (err) {
    const known = err as Error & { __notified?: boolean }
    if (!known?.__notified) {
      notifyError(known?.message || t('kafuda.config.alerts.saveFailed'))
    }
  } finally {
    saving.value = false
  }
}

const handleTestConnection = async () => {
  if (!canTest.value) return
  testing.value = true
  try {
    const payload: KafudaConfigUpdatePayload = {
      api_base_url: form.api_base_url.trim(),
    }
    if (form.api_key.trim()) {
      payload.api_key = form.api_key.trim()
    } else if (form.clear_api_key) {
      payload.clear_api_key = true
    }
    const res = await adminAPI.testKafudaConfig(payload)
    const data = res.data?.data as {
      enabled?: boolean
      options_count?: number
      cards?: KafudaCardProduct[]
    } | undefined
    if (data && Array.isArray(data.cards)) {
      cards.value = data.cards
    }
    notifySuccess(
      t('kafuda.config.alerts.testSuccess', {
        status: data?.enabled
          ? t('kafuda.config.alerts.catalogEnabledText')
          : t('kafuda.config.alerts.catalogDisabledText'),
        cards: Array.isArray(data?.cards) ? data.cards.length : 0,
        options: Number(data?.options_count || 0),
      })
    )
  } catch (err) {
    const known = err as Error & { __notified?: boolean }
    if (!known?.__notified) {
      notifyError(known?.message || t('kafuda.config.alerts.testFailed'))
    }
  } finally {
    testing.value = false
  }
}

const handleSyncCatalog = async () => {
  if (!canSync.value) return
  syncing.value = true
  try {
    await adminAPI.syncKafudaCatalog()
    notifySuccess(t('kafuda.config.alerts.syncSuccess'))
    await loadConfig()
  } catch (err) {
    const known = err as Error & { __notified?: boolean }
    if (!known?.__notified) {
      notifyError(known?.message || t('kafuda.config.alerts.syncFailed'))
    }
  } finally {
    syncing.value = false
  }
}

const selectCard = (code: string) => {
  form.product_code = code
}

onMounted(() => {
  loadConfig()
})
</script>

<template>
  <div class="space-y-6">
    <!-- Header -->
    <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <div class="space-y-1">
        <div class="flex flex-wrap items-center gap-2">
          <h1 class="text-2xl font-semibold tracking-tight">{{ t('kafuda.config.title') }}</h1>
          <Badge :variant="status.config_source === 'error' ? 'destructive' : 'secondary'">
            {{ sourceBadgeText }}
          </Badge>
          <Badge v-if="status.version > 0" variant="outline">
            {{ t('kafuda.config.versionLabel', { version: status.version }) }}
          </Badge>
        </div>
        <p class="text-sm text-muted-foreground">{{ t('kafuda.config.subtitle') }}</p>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <Button type="button" variant="outline" size="sm" :disabled="loading" @click="loadConfig">
          <RefreshCw class="mr-1.5 h-4 w-4" :class="{ 'animate-spin': loading }" />
          {{ t('kafuda.config.actions.reload') }}
        </Button>
        <Button
          v-if="canTest"
          type="button"
          variant="outline"
          size="sm"
          :disabled="testing || loading"
          @click="handleTestConnection"
        >
          {{ testing ? t('kafuda.config.actions.testing') : t('kafuda.config.actions.test') }}
        </Button>
        <Button
          v-if="canSync"
          type="button"
          variant="outline"
          size="sm"
          :disabled="syncing || loading || !status.configured"
          @click="handleSyncCatalog"
        >
          {{ syncing ? t('kafuda.config.actions.syncing') : t('kafuda.config.actions.syncCatalog') }}
        </Button>
        <Button
          v-if="canUpdate"
          type="button"
          size="sm"
          :disabled="saving || loading"
          @click="handleSave"
        >
          {{ saving ? t('kafuda.config.actions.saving') : t('kafuda.config.actions.save') }}
        </Button>
      </div>
    </div>

    <!-- Fail-closed error banner -->
    <div
      v-if="status.config_source === 'error'"
      class="flex items-start gap-3 rounded-lg border border-destructive/40 bg-destructive/10 p-4 text-sm text-destructive"
      role="alert"
    >
      <ShieldAlert class="mt-0.5 h-5 w-5 shrink-0" />
      <p>{{ t('kafuda.config.status.loadErrorHint') }}</p>
    </div>

    <!-- Runtime readiness cards -->
    <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <div class="rounded-lg border bg-card p-4 space-y-1.5">
        <p class="text-xs font-medium text-muted-foreground">{{ t('kafuda.config.status.gatewayLabel') }}</p>
        <div class="flex items-center gap-2">
          <Badge :variant="status.configured ? 'default' : status.gateway_config_invalid ? 'destructive' : 'secondary'">
            {{
              status.configured
                ? t('kafuda.config.status.gatewayReady')
                : status.gateway_config_invalid
                  ? t('kafuda.config.status.gatewayInvalid')
                  : t('kafuda.config.status.gatewayUnconfigured')
            }}
          </Badge>
        </div>
      </div>

      <div class="rounded-lg border bg-card p-4 space-y-1.5">
        <p class="text-xs font-medium text-muted-foreground">{{ t('kafuda.config.status.workerLabel') }}</p>
        <div class="flex items-center gap-2">
          <Badge :variant="status.worker_ready ? 'default' : 'secondary'">
            {{ status.worker_ready ? t('kafuda.config.status.workerReady') : t('kafuda.config.status.workerNotReady') }}
          </Badge>
        </div>
      </div>

      <div class="rounded-lg border bg-card p-4 space-y-1.5">
        <p class="text-xs font-medium text-muted-foreground">{{ t('kafuda.config.status.catalogLabel') }}</p>
        <div class="flex items-center gap-2">
          <Badge :variant="!status.catalog_stale && status.catalog_enabled ? 'default' : 'secondary'">
            {{
              !status.catalog_stale
                ? t('kafuda.config.status.catalogFresh', { count: status.catalog_options_count })
                : t('kafuda.config.status.catalogStale')
            }}
          </Badge>
        </div>
        <p v-if="status.catalog_synced_at" class="text-xs text-muted-foreground">
          {{ t('kafuda.lastSync', { time: formatDate(status.catalog_synced_at) }) }}
        </p>
      </div>

      <div class="rounded-lg border bg-card p-4 space-y-1.5">
        <p class="text-xs font-medium text-muted-foreground">{{ t('kafuda.config.status.salesLabel') }}</p>
        <div class="flex items-center gap-2">
          <Badge :variant="!status.sales_block_reason ? 'default' : 'secondary'">
            {{ !status.sales_block_reason ? t('kafuda.config.status.salesReady') : t('kafuda.config.status.salesBlocked') }}
          </Badge>
        </div>
      </div>
    </div>

    <!-- Sales block reason / ready banner -->
    <div
      v-if="salesBlockMessage"
      role="status"
      class="flex items-start gap-2.5 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300"
    >
      <ShieldAlert class="mt-0.5 h-4 w-4 shrink-0" />
      <span>{{ salesBlockMessage }}</span>
    </div>
    <div
      v-else-if="!loading"
      role="status"
      class="flex items-center gap-2.5 rounded-lg border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300"
    >
      <CheckCircle2 class="h-4 w-4 shrink-0" />
      <span>{{ t('kafuda.config.status.salesReady') }}</span>
    </div>

    <!-- Section 1: Connection & Credentials -->
    <div class="rounded-lg border bg-card p-6 space-y-5">
      <div>
        <h2 class="text-base font-semibold">{{ t('kafuda.config.connection.title') }}</h2>
        <p class="mt-1 text-xs text-muted-foreground">{{ t('kafuda.config.connection.subtitle') }}</p>
      </div>

      <div class="grid gap-5 md:grid-cols-2">
        <div class="space-y-1.5">
          <Label for="kafuda-api-base-url">{{ t('kafuda.config.connection.apiBaseUrl') }}</Label>
          <Input
            id="kafuda-api-base-url"
            v-model="form.api_base_url"
            :disabled="!canUpdate"
            :placeholder="t('kafuda.config.connection.apiBaseUrlPlaceholder')"
          />
          <p class="text-xs text-muted-foreground">{{ t('kafuda.config.connection.apiBaseUrlHint') }}</p>
        </div>

        <div class="space-y-1.5">
          <div class="flex items-center justify-between gap-2">
            <Label for="kafuda-api-key">{{ t('kafuda.config.connection.apiKey') }}</Label>
            <Badge :variant="status.has_api_key ? 'default' : 'secondary'" class="text-[11px]">
              {{
                status.has_api_key
                  ? t('kafuda.config.connection.apiKeyConfiguredBadge')
                  : t('kafuda.config.connection.apiKeyUnconfiguredBadge')
              }}
            </Badge>
          </div>
          <div class="relative">
            <Input
              id="kafuda-api-key"
              v-model="form.api_key"
              :type="showApiKey ? 'text' : 'password'"
              :disabled="!canUpdate || form.clear_api_key"
              autocomplete="new-password"
              class="pr-10"
              :placeholder="
                status.has_api_key
                  ? t('kafuda.config.connection.apiKeyConfiguredPlaceholder')
                  : t('kafuda.config.connection.apiKeyPlaceholder')
              "
            />
            <button
              type="button"
              class="absolute right-2.5 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              @click="showApiKey = !showApiKey"
            >
              <EyeOff v-if="showApiKey" class="h-4 w-4" />
              <Eye v-else class="h-4 w-4" />
            </button>
          </div>
          <p class="text-xs text-muted-foreground">{{ t('kafuda.config.connection.apiKeyHint') }}</p>
          <div v-if="status.has_api_key && canUpdate" class="flex items-center gap-2 pt-1">
            <Checkbox
              id="kafuda-clear-api-key"
              :model-value="form.clear_api_key"
              @update:model-value="(val) => { form.clear_api_key = Boolean(val); if (form.clear_api_key) form.api_key = '' }"
            />
            <Label for="kafuda-clear-api-key" class="text-xs font-normal text-muted-foreground cursor-pointer">
              {{ t('kafuda.config.connection.clearApiKey') }}
            </Label>
          </div>
        </div>
      </div>
    </div>

    <!-- Section 2: Merchant Policy & Sales Switch -->
    <div class="rounded-lg border bg-card p-6 space-y-5">
      <div>
        <h2 class="text-base font-semibold">{{ t('kafuda.config.policy.title') }}</h2>
        <p class="mt-1 text-xs text-muted-foreground">{{ t('kafuda.config.policy.subtitle') }}</p>
      </div>

      <div class="grid gap-5 md:grid-cols-3">
        <div class="space-y-1.5">
          <Label for="kafuda-product-code">{{ t('kafuda.config.policy.productCode') }}</Label>
          <Input
            id="kafuda-product-code"
            v-model="form.product_code"
            :disabled="!canUpdate"
            :placeholder="t('kafuda.config.policy.productCodePlaceholder')"
          />
          <p class="text-xs text-muted-foreground">{{ t('kafuda.config.policy.productCodeHint') }}</p>
        </div>

        <div class="space-y-1.5">
          <Label for="kafuda-max-cost-cents">{{ t('kafuda.config.policy.maxCostCents') }}</Label>
          <Input
            id="kafuda-max-cost-cents"
            v-model.number="form.max_cost_cents"
            type="number"
            min="0"
            max="100000000"
            :disabled="!canUpdate"
          />
          <p class="text-xs text-muted-foreground">
            {{ t('kafuda.config.policy.maxCostHint', { usd: maxCostUSD }) }}
          </p>
        </div>

        <div class="space-y-1.5">
          <Label for="kafuda-allowed-kinds">{{ t('kafuda.config.policy.allowedKinds') }}</Label>
          <Input
            id="kafuda-allowed-kinds"
            v-model="form.allowed_kinds_text"
            :disabled="!canUpdate"
            :placeholder="t('kafuda.config.policy.allowedKindsPlaceholder')"
          />
          <p class="text-xs text-muted-foreground">{{ t('kafuda.config.policy.allowedKindsHint') }}</p>
        </div>
      </div>

      <div class="grid gap-4 border-t pt-4 md:grid-cols-2">
        <div class="flex items-start justify-between gap-4 rounded-md border p-4">
          <div class="space-y-1">
            <Label class="text-sm font-medium">{{ t('kafuda.config.policy.referenceLookupVerified') }}</Label>
            <p class="text-xs text-muted-foreground">{{ t('kafuda.config.policy.referenceLookupVerifiedHint') }}</p>
          </div>
          <Switch v-model="form.reference_lookup_verified" :disabled="!canUpdate" />
        </div>

        <div class="flex items-start justify-between gap-4 rounded-md border p-4">
          <div class="space-y-1">
            <Label class="text-sm font-medium">{{ t('kafuda.config.policy.salesEnabled') }}</Label>
            <p class="text-xs text-muted-foreground">{{ t('kafuda.config.policy.salesEnabledHint') }}</p>
          </div>
          <Switch v-model="form.sales_enabled" :disabled="!canUpdate" />
        </div>
      </div>
    </div>

    <!-- Upstream Cards Table (when available from sync or test) -->
    <div v-if="cards.length > 0" class="rounded-lg border bg-card p-6 space-y-4">
      <div>
        <h2 class="text-base font-semibold">{{ t('kafuda.config.cards.title') }}</h2>
        <p class="mt-1 text-xs text-muted-foreground">{{ t('kafuda.config.cards.subtitle') }}</p>
      </div>
      <div class="overflow-x-auto rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{{ t('kafuda.config.cards.code') }}</TableHead>
              <TableHead>{{ t('kafuda.config.cards.name') }}</TableHead>
              <TableHead>{{ t('kafuda.config.cards.openFee') }}</TableHead>
              <TableHead>{{ t('kafuda.config.cards.minAmount') }}</TableHead>
              <TableHead>{{ t('kafuda.config.cards.status') }}</TableHead>
              <TableHead class="text-right">{{ t('kafuda.config.cards.action') }}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow v-for="card in cards" :key="card.product_code">
              <TableCell class="font-mono text-xs">{{ card.product_code }}</TableCell>
              <TableCell>
                <div class="flex items-center gap-2">
                  <span>{{ card.name || '-' }}</span>
                  <Badge v-if="card.gpt_recommended" variant="secondary" class="text-[11px]">
                    {{ t('kafuda.config.cards.recommended') }}
                  </Badge>
                </div>
              </TableCell>
              <TableCell>{{ formatCentsUSD(card.open_fee_cents) }}</TableCell>
              <TableCell>{{ formatCentsUSD(card.min_amount_cents) }}</TableCell>
              <TableCell>
                <Badge :variant="card.restricted ? 'destructive' : 'default'">
                  {{ card.restricted ? t('kafuda.config.cards.restricted') : t('kafuda.config.cards.available') }}
                </Badge>
              </TableCell>
              <TableCell class="text-right">
                <Button
                  type="button"
                  size="sm"
                  :variant="form.product_code === card.product_code ? 'default' : 'outline'"
                  :disabled="!canUpdate || card.restricted"
                  @click="selectCard(card.product_code)"
                >
                  {{
                    form.product_code === card.product_code
                      ? t('kafuda.config.actions.selectedCard')
                      : t('kafuda.config.actions.useCard')
                  }}
                </Button>
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </div>
    </div>

    <!-- Section 3: Collapsible Advanced Settings -->
    <div class="rounded-lg border bg-card p-6 space-y-4">
      <div class="flex items-center justify-between gap-4">
        <div>
          <h2 class="text-base font-semibold">{{ t('kafuda.config.advanced.title') }}</h2>
          <p class="mt-1 text-xs text-muted-foreground">{{ t('kafuda.config.advanced.subtitle') }}</p>
        </div>
        <Button type="button" variant="ghost" size="sm" @click="showAdvanced = !showAdvanced">
          <ChevronDown v-if="showAdvanced" class="mr-1 h-4 w-4" />
          <ChevronRight v-else class="mr-1 h-4 w-4" />
          {{ showAdvanced ? t('kafuda.config.advanced.toggleHide') : t('kafuda.config.advanced.toggleShow') }}
        </Button>
      </div>

      <div v-if="showAdvanced" class="space-y-4 border-t pt-4">
        <div class="grid gap-5 md:grid-cols-2">
          <div class="space-y-1.5">
            <Label for="kafuda-catalog-ttl">{{ t('kafuda.config.advanced.catalogTtlSeconds') }}</Label>
            <Input
              id="kafuda-catalog-ttl"
              v-model.number="form.catalog_ttl_seconds"
              type="number"
              min="10"
              max="86400"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('kafuda.config.advanced.catalogTtlHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="kafuda-catalog-refresh">{{ t('kafuda.config.advanced.catalogRefreshIntervalSeconds') }}</Label>
            <Input
              id="kafuda-catalog-refresh"
              v-model.number="form.catalog_refresh_interval_seconds"
              type="number"
              min="-1"
              max="86400"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('kafuda.config.advanced.catalogRefreshHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="kafuda-poll-interval">{{ t('kafuda.config.advanced.pollIntervalSeconds') }}</Label>
            <Input
              id="kafuda-poll-interval"
              v-model.number="form.poll_interval_seconds"
              type="number"
              min="5"
              max="900"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('kafuda.config.advanced.pollIntervalHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="kafuda-poll-review-after">{{ t('kafuda.config.advanced.pollReviewAfterSeconds') }}</Label>
            <Input
              id="kafuda-poll-review-after"
              v-model.number="form.poll_review_after_seconds"
              type="number"
              min="60"
              max="604800"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('kafuda.config.advanced.pollReviewAfterHint') }}</p>
          </div>
        </div>

        <div class="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-700 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300">
          {{ t('kafuda.config.advanced.workerRestartHint') }}
        </div>
      </div>
    </div>
  </div>
</template>
