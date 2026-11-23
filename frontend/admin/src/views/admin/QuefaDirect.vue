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
import { RefreshCw, CheckCircle2, ShieldAlert } from 'lucide-vue-next'

const { t, te } = useI18n()
const authStore = useAdminAuthStore()

const loading = ref(false)
const saving = ref(false)
const testing = ref(false)
const syncing = ref(false)

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
  poll_interval_seconds: 10,
  poll_review_after_seconds: 86400,
  webhook_max_skew_seconds: 300,
})

const canUpdate = computed(() => authStore.hasPermission('PUT:/admin/quefa/config'))
const canTest = computed(() => authStore.hasPermission('POST:/admin/quefa/config/test'))
const canSync = computed(() => authStore.hasPermission('POST:/admin/quefa/catalog/sync'))

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
    notifyError(err?.message || t('quefa.loadFailed'))
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
    notifyError(err?.message || t('quefa.saveFailed'))
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
    notifyError(err?.message || t('quefa.testFailed'))
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
    notifyError(err?.message || t('quefa.syncFailed'))
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

    <div class="rounded-xl border border-border bg-card p-4 flex flex-wrap items-center justify-between gap-3">
      <div class="flex items-center gap-2">
        <CheckCircle2 v-if="meta.ready" class="h-5 w-5 text-emerald-600" />
        <ShieldAlert v-else class="h-5 w-5 text-amber-500" />
        <span class="text-sm font-medium">
          {{ meta.ready ? t('quefa.readinessReady') : t('quefa.readinessBlocked') }}
        </span>
      </div>
      <div v-if="meta.block_reasons.length > 0" class="flex flex-wrap gap-1.5">
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

      <div class="grid grid-cols-1 md:grid-cols-4 gap-4">
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.catalogTtl') }}</Label>
          <Input v-model.number="form.catalog_ttl_seconds" type="number" min="10" max="86400" />
        </div>
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.pollInterval') }}</Label>
          <Input v-model.number="form.poll_interval_seconds" type="number" min="5" max="900" />
        </div>
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.pollReviewAfter') }}</Label>
          <Input v-model.number="form.poll_review_after_seconds" type="number" min="60" max="604800" />
        </div>
        <div class="space-y-1.5">
          <Label>{{ t('quefa.fields.webhookMaxSkew') }}</Label>
          <Input v-model.number="form.webhook_max_skew_seconds" type="number" min="30" max="3600" />
        </div>
      </div>

      <div class="flex items-center justify-between rounded-lg border border-border p-3">
        <div>
          <div class="text-sm font-medium">{{ t('quefa.fields.salesEnabled') }}</div>
          <p class="text-xs text-muted-foreground">{{ t('quefa.salesEnabledHint') }}</p>
        </div>
        <Switch :model-value="form.sales_enabled" @update:model-value="(val: boolean) => (form.sales_enabled = val)" />
      </div>
    </div>

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

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{{ t('quefa.table.productCode') }}</TableHead>
            <TableHead>{{ t('quefa.table.title') }}</TableHead>
            <TableHead>{{ t('quefa.table.supplyPrice') }}</TableHead>
            <TableHead>{{ t('quefa.table.minSalePrice') }}</TableHead>
            <TableHead>{{ t('quefa.table.currency') }}</TableHead>
            <TableHead>{{ t('quefa.table.status') }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow v-if="products.length === 0">
            <TableCell colspan="6" class="text-center text-muted-foreground py-6">
              {{ t('quefa.catalogEmpty') }}
            </TableCell>
          </TableRow>
          <TableRow v-for="item in products" :key="item.product_code">
            <TableCell class="font-mono text-xs">{{ item.product_code }}</TableCell>
            <TableCell>{{ item.title }}</TableCell>
            <TableCell>{{ item.supply_price }}</TableCell>
            <TableCell>{{ item.min_sale_price || '-' }}</TableCell>
            <TableCell>{{ item.currency }}</TableCell>
            <TableCell>
              <Badge :variant="item.available ? 'default' : 'secondary'">
                {{ item.available ? t('quefa.available') : t('quefa.unavailable') }}
              </Badge>
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
    </div>
  </div>
</template>
