<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  adminAPI,
  type QuefaCatalogProduct,
  type QuefaConfigData,
  type QuefaConfigUpdatePayload,
} from '@/api/admin'
import { useAdminAuthStore } from '@/stores/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Badge } from '@/components/ui/badge'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
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
import {
  resolveQuefaDeliveryModeKey,
  resolveQuefaGroupKey,
  resolveQuefaGroupLabel,
  sortQuefaGroupKeys,
} from '@/utils/quefaCatalog'
import { RefreshCw, CheckCircle2, ShieldAlert, Copy, Check, Search, ChevronDown, ChevronRight } from 'lucide-vue-next'

const { t, te, locale } = useI18n()
const authStore = useAdminAuthStore()

const loading = ref(false)
const saving = ref(false)
const testing = ref(false)
const syncing = ref(false)
const webhookCopied = ref(false)
const showAdvanced = ref(false)

const activeGroup = ref('all')
const statusFilter = ref<'all' | 'sellable' | 'quote_required' | 'unavailable'>('all')
const searchQuery = ref('')

const meta = reactive({
  source: 'config_file',
  version: 0,
  updated_at: '',
  client_secret_configured: false,
  webhook_secret_configured: false,
  ready: false,
  block_reasons: [] as string[],
  catalog_stale: true,
  catalog_synced_at: '',
  catalog_revision: 0,
  catalog_last_error: '',
})

const products = ref<QuefaCatalogProduct[]>([])

const form = reactive({
  api_base_url: 'https://tibo.ink',
  partner_id: '',
  key_id: '',
  client_secret: '',
  clear_client_secret: false,
  webhook_secret: '',
  clear_webhook_secret: false,
  collection_mode: 'platform_collect' as 'platform_collect' | 'agent_collect',
  sales_enabled: false,
  allowed_pay_domains_text: '',
  catalog_ttl_seconds: 3600,
  catalog_refresh_interval_seconds: 0,
  poll_interval_seconds: 10,
  poll_review_after_seconds: 86400,
  webhook_max_skew_seconds: 300,
})

const canUpdate = computed(() => authStore.hasPermission('PUT:/admin/quefa/config'))
const canTest = computed(() => authStore.hasPermission('POST:/admin/quefa/config/test'))
const canSync = computed(() => authStore.hasPermission('POST:/admin/quefa/catalog/sync'))

const webhookUrl = computed(() => {
  if (typeof window !== 'undefined' && window.location?.origin) {
    return `${window.location.origin}/api/v1/webhooks/quefa`
  }
  return '/api/v1/webhooks/quefa'
})

const groupLabels = computed(() => ({
  ph: t('quefa.groups.ph'),
  channel2: t('quefa.groups.channel2'),
  egypt: t('quefa.groups.egypt'),
  codex: t('quefa.groups.codex'),
  other: t('quefa.groups.other'),
}))

const isProductSellable = (item: QuefaCatalogProduct) =>
  Boolean(item.available && !item.quote_required && Number(item.supply_price || 0) > 0)

const sellableCount = computed(() => products.value.filter(isProductSellable).length)

const catalogGroups = computed(() => {
  const counts = new Map<string, number>()
  for (const item of products.value) {
    const key = resolveQuefaGroupKey(item)
    counts.set(key, (counts.get(key) ?? 0) + 1)
  }
  const ordered = sortQuefaGroupKeys(counts.keys())
  return [
    { value: 'all', label: `${t('quefa.groups.all')} (${products.value.length})` },
    ...ordered.map((key) => ({
      value: key,
      label: `${resolveQuefaGroupLabel(key, locale.value, groupLabels.value)} (${counts.get(key) ?? 0})`,
    })),
  ]
})

const filteredProducts = computed(() => {
  const q = searchQuery.value.trim().toLowerCase()
  return products.value.filter((item) => {
    if (activeGroup.value !== 'all' && resolveQuefaGroupKey(item) !== activeGroup.value) {
      return false
    }
    if (statusFilter.value === 'sellable' && !isProductSellable(item)) return false
    if (statusFilter.value === 'quote_required' && !item.quote_required) return false
    if (statusFilter.value === 'unavailable' && item.available) return false
    if (q) {
      const code = String(item.product_code || '').toLowerCase()
      const title = String(item.title || '').toLowerCase()
      if (!code.includes(q) && !title.includes(q)) return false
    }
    return true
  })
})

const resetCatalogFilters = () => {
  activeGroup.value = 'all'
  statusFilter.value = 'all'
  searchQuery.value = ''
}

const productGroupText = (item: QuefaCatalogProduct) =>
  resolveQuefaGroupLabel(resolveQuefaGroupKey(item), locale.value, groupLabels.value)

const productDeliveryText = (item: QuefaCatalogProduct) =>
  t(`quefa.deliveryModes.${resolveQuefaDeliveryModeKey(item)}`)

const copyWebhookUrl = async () => {
  try {
    await navigator.clipboard.writeText(webhookUrl.value)
    webhookCopied.value = true
    setTimeout(() => {
      webhookCopied.value = false
    }, 2000)
  } catch {
    // Ignore clipboard rejection in restricted contexts.
  }
}

const parseDomains = (raw: string): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const part of raw.split(/[\n,]/)) {
    const d = part.trim().toLowerCase()
    if (!d || seen.has(d)) continue
    seen.add(d)
    out.push(d)
  }
  return out
}

const blockReasonLabel = (code: string) => {
  const key = `quefa.blockReasons.${code}`
  return te(key) ? t(key) : code
}

const applyConfig = (cfg: QuefaConfigData, readiness?: { ready: boolean; block_reasons?: string[] }) => {
  meta.source = cfg.source || 'config_file'
  meta.version = Number(cfg.version || 0)
  meta.updated_at = cfg.updated_at || ''
  meta.client_secret_configured = Boolean(cfg.client_secret_configured)
  meta.webhook_secret_configured = Boolean(cfg.webhook_secret_configured)
  if (readiness) {
    meta.ready = Boolean(readiness.ready)
    meta.block_reasons = readiness.block_reasons || []
  }

  form.api_base_url = cfg.api_base_url || 'https://tibo.ink'
  form.partner_id = cfg.partner_id || ''
  form.key_id = cfg.key_id || ''
  form.client_secret = ''
  form.clear_client_secret = false
  form.webhook_secret = ''
  form.clear_webhook_secret = false
  form.collection_mode = cfg.collection_mode === 'agent_collect' ? 'agent_collect' : 'platform_collect'
  form.sales_enabled = Boolean(cfg.sales_enabled)
  form.allowed_pay_domains_text = (cfg.allowed_pay_domains || []).join('\n')
  form.catalog_ttl_seconds = Number(cfg.catalog_ttl_seconds || 3600)
  form.catalog_refresh_interval_seconds =
    cfg.catalog_refresh_interval_seconds !== undefined && cfg.catalog_refresh_interval_seconds !== null
      ? Number(cfg.catalog_refresh_interval_seconds)
      : 0
  form.poll_interval_seconds = Number(cfg.poll_interval_seconds || 10)
  form.poll_review_after_seconds = Number(cfg.poll_review_after_seconds || 86400)
  form.webhook_max_skew_seconds = Number(cfg.webhook_max_skew_seconds || 300)
}

const loadAll = async () => {
  loading.value = true
  try {
    const [cfgRes, catRes] = await Promise.all([
      adminAPI.getQuefaConfig(),
      adminAPI.getQuefaCatalog().catch(() => null),
    ])
    const cfgPayload = cfgRes.data?.data
    if (cfgPayload?.config) {
      applyConfig(cfgPayload.config, cfgPayload.readiness)
    }
    const catPayload = catRes?.data?.data
    if (catPayload) {
      products.value = Array.isArray(catPayload.products) ? catPayload.products : []
      meta.catalog_stale = Boolean(catPayload.stale)
      meta.catalog_synced_at = catPayload.synced_at || ''
      meta.catalog_revision = Number(catPayload.revision || 0)
      meta.catalog_last_error = catPayload.last_error_code || ''
    }
  } catch (err: any) {
    if (!err?.__notified) notifyError(err?.message || t('quefa.loadFailed'))
  } finally {
    loading.value = false
  }
}

const buildPatch = (): QuefaConfigUpdatePayload => {
  const patch: QuefaConfigUpdatePayload = {
    api_base_url: form.api_base_url.trim(),
    partner_id: form.partner_id.trim(),
    key_id: form.key_id.trim(),
    collection_mode: form.collection_mode,
    sales_enabled: form.sales_enabled,
    allowed_pay_domains: parseDomains(form.allowed_pay_domains_text),
    catalog_ttl_seconds: Number(form.catalog_ttl_seconds),
    catalog_refresh_interval_seconds: Number(form.catalog_refresh_interval_seconds),
    poll_interval_seconds: Number(form.poll_interval_seconds),
    poll_review_after_seconds: Number(form.poll_review_after_seconds),
    webhook_max_skew_seconds: Number(form.webhook_max_skew_seconds),
  }
  if (form.clear_client_secret) {
    patch.clear_client_secret = true
  } else if (form.client_secret.trim()) {
    patch.client_secret = form.client_secret.trim()
  }
  if (form.clear_webhook_secret) {
    patch.clear_webhook_secret = true
  } else if (form.webhook_secret.trim()) {
    patch.webhook_secret = form.webhook_secret.trim()
  }
  return patch
}

const handleSave = async () => {
  saving.value = true
  try {
    const res = await adminAPI.updateQuefaConfig(buildPatch())
    const payload = res.data?.data
    if (payload?.config) {
      applyConfig(payload.config, payload.readiness)
    }
    notifySuccess(t('quefa.saveSuccess'))
  } catch (err: any) {
    if (!err?.__notified) notifyError(err?.message || t('quefa.saveFailed'))
  } finally {
    saving.value = false
  }
}

const handleTest = async () => {
  testing.value = true
  try {
    const res = await adminAPI.testQuefaConfig(buildPatch())
    const count = res.data?.data?.product_count ?? 0
    notifySuccess(t('quefa.testSuccess', { count }))
  } catch (err: any) {
    if (!err?.__notified) notifyError(err?.message || t('quefa.testFailed'))
  } finally {
    testing.value = false
  }
}

const handleSyncCatalog = async () => {
  syncing.value = true
  try {
    const res = await adminAPI.syncQuefaCatalog()
    const payload = res.data?.data
    if (payload) {
      products.value = Array.isArray(payload.products) ? payload.products : []
      meta.catalog_stale = Boolean(payload.stale)
      meta.catalog_synced_at = payload.synced_at || ''
      meta.catalog_revision = Number(payload.revision || 0)
      meta.catalog_last_error = payload.last_error_code || ''
      if (payload.readiness) {
        meta.ready = Boolean(payload.readiness.ready)
        meta.block_reasons = payload.readiness.block_reasons || []
      }
    }
    notifySuccess(t('quefa.syncSuccess'))
  } catch (err: any) {
    if (!err?.__notified) notifyError(err?.message || t('quefa.syncFailed'))
  } finally {
    syncing.value = false
  }
}

onMounted(() => {
  void loadAll()
})
</script>

<template>
  <div class="space-y-6">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 class="text-xl font-semibold">{{ t('quefa.title') }}</h1>
        <p class="text-sm text-muted-foreground mt-1">{{ t('quefa.subtitle') }}</p>
      </div>
      <div class="flex items-center gap-2">
        <Button v-if="canTest" variant="outline" :disabled="testing || loading" @click="handleTest">
          {{ testing ? t('quefa.testing') : t('quefa.testConnection') }}
        </Button>
        <Button v-if="canSync" variant="outline" :disabled="syncing || loading" @click="handleSyncCatalog">
          <RefreshCw class="h-4 w-4 mr-1.5" :class="{ 'animate-spin': syncing }" />
          {{ t('quefa.syncCatalog') }}
        </Button>
        <Button v-if="canUpdate" :disabled="saving || loading" @click="handleSave">
          {{ saving ? t('quefa.saving') : t('quefa.saveConfig') }}
        </Button>
      </div>
    </div>

    <!-- 4-Card Overview Strip (aligned with tibo.ink API access workbench) -->
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <div class="rounded-xl border border-border bg-card p-4 space-y-1.5">
        <div class="text-xs text-muted-foreground">{{ t('quefa.overview.readiness') }}</div>
        <div class="flex items-center gap-2">
          <CheckCircle2 v-if="meta.ready" class="h-4 w-4 text-emerald-600 shrink-0" />
          <ShieldAlert v-else class="h-4 w-4 text-amber-500 shrink-0" />
          <span class="text-sm font-semibold">
            {{ meta.ready ? t('quefa.readinessReady') : t('quefa.readinessBlocked') }}
          </span>
        </div>
      </div>

      <div class="rounded-xl border border-border bg-card p-4 space-y-1.5">
        <div class="text-xs text-muted-foreground">{{ t('quefa.overview.collectionMode') }}</div>
        <div class="text-sm font-semibold truncate">
          {{ t(`quefa.collectionModes.${form.collection_mode}`) }}
        </div>
      </div>

      <div class="rounded-xl border border-border bg-card p-4 space-y-1.5">
        <div class="text-xs text-muted-foreground">{{ t('quefa.overview.sellableCount') }}</div>
        <div class="text-lg font-bold tabular-nums">
          {{ sellableCount }} <span class="text-xs font-normal text-muted-foreground">/ {{ products.length }}</span>
        </div>
      </div>

      <div class="rounded-xl border border-border bg-card p-4 space-y-1.5">
        <div class="flex items-center justify-between gap-2">
          <span class="text-xs text-muted-foreground">{{ t('quefa.overview.webhookUrl') }}</span>
          <button
            type="button"
            class="inline-flex items-center gap-1 text-[11px] font-medium text-primary hover:underline"
            @click="copyWebhookUrl"
          >
            <Check v-if="webhookCopied" class="h-3 w-3" />
            <Copy v-else class="h-3 w-3" />
            {{ webhookCopied ? t('quefa.overview.copied') : t('quefa.overview.copyWebhook') }}
          </button>
        </div>
        <div class="font-mono text-xs text-foreground truncate" :title="webhookUrl">
          {{ webhookUrl }}
        </div>
      </div>
    </div>

    <div
      v-if="meta.block_reasons.length > 0"
      class="rounded-xl border border-amber-200 bg-amber-50/70 dark:border-amber-800/60 dark:bg-amber-950/20 p-4 flex flex-wrap items-center gap-2"
    >
      <ShieldAlert class="h-4 w-4 text-amber-600 shrink-0" />
      <div class="flex flex-wrap gap-1.5">
        <Badge v-for="code in meta.block_reasons" :key="code" variant="secondary">
          {{ blockReasonLabel(code) }}
        </Badge>
      </div>
    </div>

    <div class="rounded-xl border border-border bg-card p-6 space-y-5">
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.apiBaseUrl') }}</Label>
          <Input v-model="form.api_base_url" placeholder="https://tibo.ink" />
        </div>
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.collectionMode') }}</Label>
          <Select v-model="form.collection_mode">
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="platform_collect">{{ t('quefa.collectionModes.platform_collect') }}</SelectItem>
              <SelectItem value="agent_collect">{{ t('quefa.collectionModes.agent_collect') }}</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.partnerId') }}</Label>
          <Input v-model="form.partner_id" />
        </div>
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.keyId') }}</Label>
          <Input v-model="form.key_id" />
        </div>

        <div class="space-y-1.5">
          <div class="flex items-center justify-between">
            <Label>{{ t('quefa.fields.clientSecret') }}</Label>
            <Badge :variant="meta.client_secret_configured ? 'default' : 'outline'">
              {{ meta.client_secret_configured ? t('quefa.configured') : t('quefa.notConfigured') }}
            </Badge>
          </div>
          <Input v-model="form.client_secret" type="password" :placeholder="t('quefa.secretPlaceholder')" />
        </div>

        <div class="space-y-1.5">
          <div class="flex items-center justify-between">
            <Label>{{ t('quefa.fields.webhookSecret') }}</Label>
            <Badge :variant="meta.webhook_secret_configured ? 'default' : 'outline'">
              {{ meta.webhook_secret_configured ? t('quefa.configured') : t('quefa.notConfigured') }}
            </Badge>
          </div>
          <Input v-model="form.webhook_secret" type="password" :placeholder="t('quefa.secretPlaceholder')" />
        </div>
      </div>

      <div class="space-y-1.5">
        <Label>{{ t('quefa.fields.allowedPayDomains') }}</Label>
        <textarea
          v-model="form.allowed_pay_domains_text"
          rows="3"
          class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
          :placeholder="t('quefa.allowedPayDomainsPlaceholder')"
        />
        <p class="text-xs text-muted-foreground">{{ t('quefa.allowedPayDomainsHint') }}</p>
      </div>

      <div class="flex items-center justify-between rounded-lg border border-border p-3">
        <div>
          <div class="text-sm font-medium">{{ t('quefa.fields.salesEnabled') }}</div>
          <p class="text-xs text-muted-foreground">{{ t('quefa.salesEnabledHint') }}</p>
        </div>
        <Switch :model-value="form.sales_enabled" @update:model-value="(val: boolean) => (form.sales_enabled = val)" />
      </div>
    </div>

    <!-- Catalog Snapshot (grouped by channel/region + search + status filters like tibo.ink store) -->
    <div class="rounded-xl border border-border bg-card p-6 space-y-4">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 class="text-base font-semibold">{{ t('quefa.catalogTitle') }}</h2>
          <p class="text-xs text-muted-foreground">
            {{ meta.catalog_synced_at ? `${t('quefa.lastSynced')}: ${formatDate(meta.catalog_synced_at)}` : t('quefa.neverSynced') }}
          </p>
        </div>
        <Badge :variant="meta.catalog_stale ? 'destructive' : 'default'">
          {{ meta.catalog_stale ? t('quefa.catalogStale') : t('quefa.catalogFresh') }}
        </Badge>
      </div>

      <div v-if="products.length > 0" class="space-y-3">
        <!-- Channel / Region Category Tabs -->
        <div class="flex flex-wrap gap-1.5" role="tablist">
          <button
            v-for="group in catalogGroups"
            :key="group.value"
            type="button"
            role="tab"
            :aria-selected="activeGroup === group.value"
            class="rounded-full border px-3 py-1 text-xs font-medium transition-colors"
            :class="activeGroup === group.value
              ? 'border-primary bg-primary text-primary-foreground'
              : 'border-border bg-background text-muted-foreground hover:text-foreground'"
            @click="activeGroup = group.value"
          >
            {{ group.label }}
          </button>
        </div>

        <!-- Search + Status Filter Bar -->
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div class="relative w-full sm:w-64">
            <Search class="pointer-events-none absolute left-2.5 top-2.5 h-3.5 w-3.5 text-muted-foreground" />
            <Input
              v-model="searchQuery"
              class="h-8 pl-8 text-xs"
              :placeholder="t('quefa.catalogFilters.searchPlaceholder')"
            />
          </div>
          <div class="flex flex-wrap items-center gap-1.5">
            <button
              v-for="opt in (['all', 'sellable', 'quote_required', 'unavailable'] as const)"
              :key="opt"
              type="button"
              class="rounded-md border px-2.5 py-1 text-xs font-medium transition-colors"
              :class="statusFilter === opt
                ? 'border-foreground bg-foreground text-background'
                : 'border-border bg-background text-muted-foreground hover:text-foreground'"
              @click="statusFilter = opt"
            >
              {{ t(`quefa.catalogFilters.${opt === 'quote_required' ? 'quoteRequired' : opt}`) }}
            </button>
          </div>
        </div>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{{ t('quefa.table.productCode') }}</TableHead>
            <TableHead>{{ t('quefa.table.title') }}</TableHead>
            <TableHead>{{ t('quefa.table.group') }}</TableHead>
            <TableHead>{{ t('quefa.table.deliveryMode') }}</TableHead>
            <TableHead>{{ t('quefa.table.supplyPrice') }}</TableHead>
            <TableHead>{{ t('quefa.table.minSalePrice') }}</TableHead>
            <TableHead>{{ t('quefa.table.status') }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow v-if="products.length === 0">
            <TableCell colspan="7" class="text-center text-muted-foreground py-6">
              {{ t('quefa.catalogEmpty') }}
            </TableCell>
          </TableRow>
          <TableRow v-else-if="filteredProducts.length === 0">
            <TableCell colspan="7" class="text-center text-muted-foreground py-6">
              <div class="space-y-2">
                <div>{{ t('quefa.catalogFilters.noMatches') }}</div>
                <Button type="button" size="sm" variant="outline" @click="resetCatalogFilters">
                  {{ t('quefa.catalogFilters.reset') }}
                </Button>
              </div>
            </TableCell>
          </TableRow>
          <TableRow v-for="item in filteredProducts" :key="item.product_code">
            <TableCell class="font-mono text-xs">{{ item.product_code }}</TableCell>
            <TableCell class="font-medium">{{ item.title }}</TableCell>
            <TableCell>
              <Badge variant="outline" class="text-xs">
                {{ productGroupText(item) }}
              </Badge>
            </TableCell>
            <TableCell class="text-xs text-muted-foreground">
              {{ productDeliveryText(item) }}
            </TableCell>
            <TableCell class="font-mono text-xs">
              <Badge v-if="item.quote_required" variant="secondary" class="text-amber-700 dark:text-amber-300">
                {{ t('quefa.quoteRequiredTag') }}
              </Badge>
              <span v-else>{{ item.supply_price }} {{ item.currency }}</span>
            </TableCell>
            <TableCell class="font-mono text-xs">
              {{ item.quote_required ? '-' : (item.min_sale_price || '-') }}
            </TableCell>
            <TableCell>
              <Badge v-if="isProductSellable(item)" variant="default">
                {{ t('quefa.available') }}
              </Badge>
              <Badge v-else-if="item.quote_required" variant="secondary">
                {{ t('quefa.quoteRequiredTag') }}
              </Badge>
              <Badge v-else variant="outline">
                {{ t('quefa.unavailable') }}
              </Badge>
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
    </div>

    <!-- Collapsible Advanced Cache & Polling Settings (aligned with KafudaDirect) -->
    <div class="rounded-xl border border-border bg-card p-6 space-y-4">
      <div class="flex items-center justify-between gap-4">
        <div>
          <h2 class="text-base font-semibold">{{ t('quefa.advanced.title') }}</h2>
          <p class="mt-1 text-xs text-muted-foreground">{{ t('quefa.advanced.subtitle') }}</p>
        </div>
        <Button type="button" variant="ghost" size="sm" @click="showAdvanced = !showAdvanced">
          <ChevronDown v-if="showAdvanced" class="mr-1 h-4 w-4" />
          <ChevronRight v-else class="mr-1 h-4 w-4" />
          {{ showAdvanced ? t('quefa.advanced.toggleHide') : t('quefa.advanced.toggleShow') }}
        </Button>
      </div>

      <div v-if="showAdvanced" class="space-y-4 border-t pt-4">
        <div class="grid gap-5 md:grid-cols-2">
          <div class="space-y-1.5">
            <Label for="quefa-catalog-ttl">{{ t('quefa.advanced.catalogTtlSeconds') }}</Label>
            <Input
              id="quefa-catalog-ttl"
              v-model.number="form.catalog_ttl_seconds"
              type="number"
              min="10"
              max="86400"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('quefa.advanced.catalogTtlHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="quefa-catalog-refresh">{{ t('quefa.advanced.catalogRefreshIntervalSeconds') }}</Label>
            <Input
              id="quefa-catalog-refresh"
              v-model.number="form.catalog_refresh_interval_seconds"
              type="number"
              min="-1"
              max="86400"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('quefa.advanced.catalogRefreshHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="quefa-poll-interval">{{ t('quefa.advanced.pollIntervalSeconds') }}</Label>
            <Input
              id="quefa-poll-interval"
              v-model.number="form.poll_interval_seconds"
              type="number"
              min="5"
              max="900"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('quefa.advanced.pollIntervalHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="quefa-poll-review-after">{{ t('quefa.advanced.pollReviewAfterSeconds') }}</Label>
            <Input
              id="quefa-poll-review-after"
              v-model.number="form.poll_review_after_seconds"
              type="number"
              min="60"
              max="604800"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('quefa.advanced.pollReviewAfterHint') }}</p>
          </div>

          <div class="space-y-1.5">
            <Label for="quefa-webhook-max-skew">{{ t('quefa.advanced.webhookMaxSkewSeconds') }}</Label>
            <Input
              id="quefa-webhook-max-skew"
              v-model.number="form.webhook_max_skew_seconds"
              type="number"
              min="30"
              max="3600"
              :disabled="!canUpdate"
            />
            <p class="text-xs text-muted-foreground">{{ t('quefa.advanced.webhookMaxSkewHint') }}</p>
          </div>
        </div>

        <div class="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-700 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300">
          {{ t('quefa.advanced.workerRestartHint') }}
        </div>
      </div>
    </div>
  </div>
</template>
