<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { opsAPI } from '@/api/admin/ops'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import type { OpsAlertRuntimeSettings, EmailNotificationConfig, WebhookNotifyConfig, AlertSeverity, OpsAdvancedSettings, OpsMetricThresholds } from '../types'

const { t } = useI18n()
const appStore = useAppStore()

const props = defineProps<{
  show: boolean
}>()

const emit = defineEmits<{
  close: []
  saved: []
}>()

const loading = ref(false)
const settingsLoaded = ref(false)
const saving = ref(false)

// 运行时设置
const runtimeSettings = ref<OpsAlertRuntimeSettings | null>(null)
// 邮件通知配置
const emailConfig = ref<EmailNotificationConfig | null>(null)
// Webhook 通知配置（飞书机器人）
const webhookConfig = ref<WebhookNotifyConfig | null>(null)
// Webhook 签名密钥：只写字段，空串表示保留已存值
const webhookSecretInput = ref('')
// 显式清除已保存密钥（切换到未开启签名的机器人时使用）
const webhookClearSecret = ref(false)
const sendingWebhookTest = ref(false)
// 高级设置
const advancedSettings = ref<OpsAdvancedSettings | null>(null)
// 指标阈值配置
const metricThresholds = ref<OpsMetricThresholds>({
  sla_percent_min: 99.5,
  ttft_p99_ms_max: 500,
  request_error_rate_percent_max: 5,
  upstream_error_rate_percent_max: 5
})

// Webhook 配置加载失败（如混合版本部署：新前端 + 旧后端 404）时的安全默认态
function defaultWebhookNotifyConfig(): WebhookNotifyConfig {
  return {
    enabled: false,
    provider: 'feishu',
    webhook_url: '',
    secret_configured: false,
    rate_limit_per_minute: 50,
    at_user_ids: [],
    events: { order_recharge_paid: true, order_subscription_paid: true, account_auto_paused: true, apikey_breaker_tripped: true }
  }
}

// 加载所有配置
async function loadAllSettings() {
  settingsLoaded.value = false
  loading.value = true
  try {
    const [runtime, email, advanced, thresholds, webhook] = await Promise.all([
      opsAPI.getAlertRuntimeSettings(),
      opsAPI.getEmailNotificationConfig(),
      opsAPI.getAdvancedSettings(),
      opsAPI.getMetricThresholds(),
      // 混合版本部署时该接口可能 404：回退默认配置，不阻断整个弹窗加载
      opsAPI.getWebhookNotificationConfig().catch(() => null)
    ])
    runtimeSettings.value = runtime
    emailConfig.value = email
    webhookConfig.value = webhook ?? defaultWebhookNotifyConfig()
    webhookSecretInput.value = ''
    webhookClearSecret.value = false
    advancedSettings.value = advanced
    // 兼容旧 payload：后端未返回该字段时补默认值，保证表单可绑定
    if (advancedSettings.value && !advancedSettings.value.openai_account_quota_auto_pause) {
      advancedSettings.value.openai_account_quota_auto_pause = { default_threshold_5h: 0, default_threshold_7d: 0 }
    }
    // 如果后端返回了阈值，使用后端的值；否则保持默认值
    if (thresholds && Object.keys(thresholds).length > 0) {
        metricThresholds.value = {
          sla_percent_min: thresholds.sla_percent_min ?? 99.5,
          ttft_p99_ms_max: thresholds.ttft_p99_ms_max ?? 500,
          request_error_rate_percent_max: thresholds.request_error_rate_percent_max ?? 5,
          upstream_error_rate_percent_max: thresholds.upstream_error_rate_percent_max ?? 5
        }
    }
    settingsLoaded.value = true
  } catch (err: any) {
    console.error('[OpsSettingsDialog] Failed to load settings', err)
    appStore.showError(err?.response?.data?.detail || t('admin.ops.settings.loadFailed'))
  } finally {
    loading.value = false
  }
}

// 监听弹窗打开
watch(() => props.show, (show) => {
  if (show) {
    loadAllSettings()
  }
})

// 邮件输入
const alertRecipientInput = ref('')
const reportRecipientInput = ref('')

// 严重级别选项
const severityOptions: Array<{ value: AlertSeverity | ''; label: string }> = [
  { value: '', label: t('admin.ops.email.minSeverityAll') },
  { value: 'critical', label: t('common.critical') },
  { value: 'warning', label: t('common.warning') },
  { value: 'info', label: t('common.info') }
]

// 验证邮箱
function isValidEmailAddress(email: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)
}

// 添加收件人
function addRecipient(target: 'alert' | 'report') {
  if (!emailConfig.value) return
  const raw = (target === 'alert' ? alertRecipientInput.value : reportRecipientInput.value).trim()
  if (!raw) return

  if (!isValidEmailAddress(raw)) {
    appStore.showError(t('common.invalidEmail'))
    return
  }

  const normalized = raw.toLowerCase()
  const list = target === 'alert' ? emailConfig.value.alert.recipients : emailConfig.value.report.recipients
  if (!list.includes(normalized)) {
    list.push(normalized)
  }
  if (target === 'alert') alertRecipientInput.value = ''
  else reportRecipientInput.value = ''
}

// 移除收件人
function removeRecipient(target: 'alert' | 'report', email: string) {
  if (!emailConfig.value) return
  const list = target === 'alert' ? emailConfig.value.alert.recipients : emailConfig.value.report.recipients
  const idx = list.indexOf(email)
  if (idx >= 0) list.splice(idx, 1)
}

// Webhook @用户列表：逗号分隔输入 ↔ 数组
const webhookAtUserIDsInput = computed<string>({
  get() {
    return (webhookConfig.value?.at_user_ids ?? []).join(',')
  },
  set(val: string) {
    if (!webhookConfig.value) return
    webhookConfig.value.at_user_ids = val.split(',').map((s) => s.trim()).filter(Boolean)
  }
})

// 发送 Webhook 测试消息：空 URL/secret 时后端回退到已存配置；
// 勾选清除密钥且未输入新密钥时，按无签名配置发送
async function sendWebhookTest() {
  if (sendingWebhookTest.value || !webhookConfig.value) return
  const typedSecret = webhookSecretInput.value.trim()
  sendingWebhookTest.value = true
  try {
    const result = await opsAPI.testWebhookNotification({
      webhook_url: webhookConfig.value.webhook_url.trim(),
      secret: typedSecret,
      at_user_ids: webhookConfig.value.at_user_ids,
      ignore_stored_secret: typedSecret === '' && webhookClearSecret.value
    })
    appStore.showSuccess(result.message || t('admin.ops.webhook.testSuccess'))
  } catch (err: any) {
    console.error('[OpsSettingsDialog] Failed to send webhook test', err)
    appStore.showError(err?.response?.data?.message || err?.response?.data?.detail || t('admin.ops.webhook.testFailed'))
  } finally {
    sendingWebhookTest.value = false
  }
}

// OpenAI 账号配额自动暂停：后端按 0~1 分数存储，UI 按百分比(0~100)展示
const quotaAutoPause5hPercent = computed<number | null>({
  get() {
    const v = advancedSettings.value?.openai_account_quota_auto_pause?.default_threshold_5h
    return v && v > 0 ? Math.round(v * 1000) / 10 : null
  },
  set(val) {
    if (!advancedSettings.value?.openai_account_quota_auto_pause) return
    advancedSettings.value.openai_account_quota_auto_pause.default_threshold_5h = val != null && val > 0 ? val / 100 : 0
  }
})
const quotaAutoPause7dPercent = computed<number | null>({
  get() {
    const v = advancedSettings.value?.openai_account_quota_auto_pause?.default_threshold_7d
    return v && v > 0 ? Math.round(v * 1000) / 10 : null
  },
  set(val) {
    if (!advancedSettings.value?.openai_account_quota_auto_pause) return
    advancedSettings.value.openai_account_quota_auto_pause.default_threshold_7d = val != null && val > 0 ? val / 100 : 0
  }
})

// 验证
const validation = computed(() => {
  const errors: string[] = []

  // 验证运行时设置
  if (runtimeSettings.value) {
    const evalSeconds = runtimeSettings.value.evaluation_interval_seconds
    if (!Number.isFinite(evalSeconds) || evalSeconds < 1 || evalSeconds > 86400) {
      errors.push(t('admin.ops.runtime.validation.evalIntervalRange'))
    }
  }

  // 邮件配置: 启用但无收件人时不阻断保存, 保存时会自动禁用

  // Webhook 通知配置
  if (webhookConfig.value) {
    if (webhookConfig.value.enabled && !webhookConfig.value.webhook_url.trim()) {
      errors.push(t('admin.ops.webhook.validation.urlRequired'))
    }
    if (webhookConfig.value.webhook_url.trim() && !/^https:\/\//.test(webhookConfig.value.webhook_url.trim())) {
      errors.push(t('admin.ops.webhook.validation.urlHttps'))
    }
    // 频率上限：启用时必须落在 1–90（与文案一致）；清空/越界由保存侧兜底
    if (webhookConfig.value.enabled) {
      const rate = webhookConfig.value.rate_limit_per_minute
      if (!Number.isFinite(rate) || rate < 1 || rate > 90) {
        errors.push(t('admin.ops.webhook.validation.rateLimitRange'))
      }
    }
  }

  // 验证高级设置
  if (advancedSettings.value) {
    const { error_log_retention_days, minute_metrics_retention_days, hourly_metrics_retention_days } = advancedSettings.value.data_retention
    if (error_log_retention_days < 0 || error_log_retention_days > 365) {
      errors.push(t('admin.ops.settings.validation.retentionDaysRange'))
    }
    if (minute_metrics_retention_days < 0 || minute_metrics_retention_days > 365) {
      errors.push(t('admin.ops.settings.validation.retentionDaysRange'))
    }
    if (hourly_metrics_retention_days < 0 || hourly_metrics_retention_days > 365) {
      errors.push(t('admin.ops.settings.validation.retentionDaysRange'))
    }

    const { default_threshold_5h, default_threshold_7d } = advancedSettings.value.openai_account_quota_auto_pause
    if (default_threshold_5h < 0 || default_threshold_5h > 1 || default_threshold_7d < 0 || default_threshold_7d > 1) {
      errors.push(t('admin.ops.settings.validation.openaiQuotaAutoPauseRange'))
    }
  }

  // 验证指标阈值
  if (metricThresholds.value.sla_percent_min != null && (metricThresholds.value.sla_percent_min < 0 || metricThresholds.value.sla_percent_min > 100)) {
    errors.push(t('admin.ops.settings.validation.slaMinPercentRange'))
  }
  if (metricThresholds.value.ttft_p99_ms_max != null && metricThresholds.value.ttft_p99_ms_max < 0) {
    errors.push(t('admin.ops.settings.validation.ttftP99MaxRange'))
  }
  if (metricThresholds.value.request_error_rate_percent_max != null && (metricThresholds.value.request_error_rate_percent_max < 0 || metricThresholds.value.request_error_rate_percent_max > 100)) {
    errors.push(t('admin.ops.settings.validation.requestErrorRateMaxRange'))
  }
  if (metricThresholds.value.upstream_error_rate_percent_max != null && (metricThresholds.value.upstream_error_rate_percent_max < 0 || metricThresholds.value.upstream_error_rate_percent_max > 100)) {
    errors.push(t('admin.ops.settings.validation.upstreamErrorRateMaxRange'))
  }

  return { valid: errors.length === 0, errors }
})

// Webhook 频率上限兜底：输入被清空（v-model.number 变 ''）或越界时回退默认值，
// 避免向后端发送非法数字触发绑定 400；启用态下的越界已由 validation 拦截
function normalizeWebhookRateLimit(rate: number): number {
  return Number.isFinite(rate) && rate >= 1 && rate <= 90 ? rate : 50
}

// 保存所有配置
async function saveAllSettings() {
  if (loading.value || !settingsLoaded.value || saving.value) return
  if (!validation.value.valid) {
    appStore.showError(validation.value.errors[0])
    return
  }

  saving.value = true
  // 混合版本部署时 webhook 接口可能 404：只失败这一块，不拖垮其余设置的保存
  let webhookSaveFailed = false
  try {
    // 无收件人时自动禁用邮件通知
    if (emailConfig.value) {
      if (emailConfig.value.alert.enabled && emailConfig.value.alert.recipients.length === 0) {
        emailConfig.value.alert.enabled = false
      }
      if (emailConfig.value.report.enabled && emailConfig.value.report.recipients.length === 0) {
        emailConfig.value.report.enabled = false
      }
    }
    await Promise.all([
      runtimeSettings.value ? opsAPI.updateAlertRuntimeSettings(runtimeSettings.value) : Promise.resolve(),
      emailConfig.value ? opsAPI.updateEmailNotificationConfig(emailConfig.value) : Promise.resolve(),
      advancedSettings.value ? opsAPI.updateAdvancedSettings(advancedSettings.value) : Promise.resolve(),
      opsAPI.updateMetricThresholds(metricThresholds.value),
      webhookConfig.value
        ? Promise.resolve(opsAPI.updateWebhookNotificationConfig({
            enabled: webhookConfig.value.enabled,
            webhook_url: webhookConfig.value.webhook_url.trim(),
            secret: webhookSecretInput.value.trim(),
            // 未输入新密钥且勾选清除时才清除；输入了新密钥即为替换
            clear_secret: webhookSecretInput.value.trim() === '' && webhookClearSecret.value,
            rate_limit_per_minute: normalizeWebhookRateLimit(webhookConfig.value.rate_limit_per_minute),
            at_user_ids: webhookConfig.value.at_user_ids,
            events: webhookConfig.value.events
          })).catch((err: any) => {
            webhookSaveFailed = true
            console.error('[OpsSettingsDialog] Failed to save webhook notification config', err)
          })
        : Promise.resolve()
    ])
    appStore.showSuccess(t('admin.ops.settings.saveSuccess'))
    if (webhookSaveFailed) {
      appStore.showError(t('admin.ops.webhook.saveFailed'))
    }
    emit('saved')
    emit('close')
  } catch (err: any) {
    console.error('[OpsSettingsDialog] Failed to save settings', err)
    appStore.showError(err?.response?.data?.message || err?.response?.data?.detail || t('admin.ops.settings.saveFailed'))
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <BaseDialog :show="show" :title="t('admin.ops.settings.title')" width="extra-wide" @close="emit('close')">
    <div v-if="loading" class="py-10 text-center text-sm text-gray-500">
      {{ t('common.loading') }}
    </div>

    <div v-else-if="runtimeSettings && emailConfig && advancedSettings" class="space-y-6">
      <!-- 验证错误 -->
      <div v-if="!validation.valid" class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-900/50 dark:bg-amber-900/20 dark:text-amber-200">
        <div class="font-bold">{{ t('admin.ops.settings.validation.title') }}</div>
        <ul class="mt-1 list-disc space-y-1 pl-4">
          <li v-for="msg in validation.errors" :key="msg">{{ msg }}</li>
        </ul>
      </div>

      <!-- 数据采集频率 -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.settings.dataCollection') }}</h4>
        <div>
          <label class="input-label">{{ t('admin.ops.settings.evaluationInterval') }}</label>
          <input
            v-model.number="runtimeSettings.evaluation_interval_seconds"
            type="number"
            min="1"
            max="86400"
            class="input"
          />
          <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.evaluationIntervalHint') }}</p>
        </div>
      </div>

      <!-- 预警配置 -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.settings.alertConfig') }}</h4>

        <div class="space-y-4">
          <div class="flex items-center justify-between">
            <div>
              <label class="font-medium text-gray-900 dark:text-white">{{ t('admin.ops.settings.enableAlert') }}</label>
            </div>
            <Toggle v-model="emailConfig.alert.enabled" />
          </div>

          <div v-if="emailConfig.alert.enabled">
            <label class="input-label">{{ t('admin.ops.settings.alertRecipients') }}</label>
            <div class="flex gap-2">
              <input
                v-model="alertRecipientInput"
                type="email"
                class="input"
                :placeholder="t('admin.ops.settings.emailPlaceholder')"
                @keydown.enter.prevent="addRecipient('alert')"
              />
              <button class="btn btn-secondary whitespace-nowrap" type="button" @click="addRecipient('alert')">
                {{ t('common.add') }}
              </button>
            </div>
            <div class="mt-2 flex flex-wrap gap-2">
              <span
                v-for="email in emailConfig.alert.recipients"
                :key="email"
                class="inline-flex items-center gap-2 rounded-full bg-blue-100 px-3 py-1 text-xs font-medium text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"
              >
                {{ email }}
                <button type="button" class="text-blue-700/80 hover:text-blue-900" @click="removeRecipient('alert', email)">×</button>
              </span>
            </div>
            <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.ops.settings.recipientsHint') }}
            </p>
          </div>

          <div v-if="emailConfig.alert.enabled">
            <label class="input-label">{{ t('admin.ops.settings.minSeverity') }}</label>
            <Select v-model="emailConfig.alert.min_severity" :options="severityOptions" />
          </div>
        </div>
      </div>

      <!-- 评估报告配置 -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.settings.reportConfig') }}</h4>

        <div class="space-y-4">
          <div class="flex items-center justify-between">
            <div>
              <label class="font-medium text-gray-900 dark:text-white">{{ t('admin.ops.settings.enableReport') }}</label>
            </div>
            <Toggle v-model="emailConfig.report.enabled" />
          </div>

          <div v-if="emailConfig.report.enabled">
            <label class="input-label">{{ t('admin.ops.settings.reportRecipients') }}</label>
            <div class="flex gap-2">
              <input
                v-model="reportRecipientInput"
                type="email"
                class="input"
                :placeholder="t('admin.ops.settings.emailPlaceholder')"
                @keydown.enter.prevent="addRecipient('report')"
              />
              <button class="btn btn-secondary whitespace-nowrap" type="button" @click="addRecipient('report')">
                {{ t('common.add') }}
              </button>
            </div>
            <div class="mt-2 flex flex-wrap gap-2">
              <span
                v-for="email in emailConfig.report.recipients"
                :key="email"
                class="inline-flex items-center gap-2 rounded-full bg-blue-100 px-3 py-1 text-xs font-medium text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"
              >
                {{ email }}
                <button type="button" class="text-blue-700/80 hover:text-blue-900" @click="removeRecipient('report', email)">×</button>
              </span>
            </div>
            <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.ops.settings.recipientsHint') }}
            </p>
          </div>

          <div v-if="emailConfig.report.enabled" class="grid grid-cols-1 gap-4 md:grid-cols-2">
            <div class="flex items-center justify-between">
              <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.dailySummary') }}</label>
              <Toggle v-model="emailConfig.report.daily_summary_enabled" />
            </div>
            <div v-if="emailConfig.report.daily_summary_enabled">
              <input v-model="emailConfig.report.daily_summary_schedule" type="text" class="input" placeholder="0 9 * * *" />
            </div>
            <div class="flex items-center justify-between">
              <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.weeklySummary') }}</label>
              <Toggle v-model="emailConfig.report.weekly_summary_enabled" />
            </div>
            <div v-if="emailConfig.report.weekly_summary_enabled">
              <input v-model="emailConfig.report.weekly_summary_schedule" type="text" class="input" placeholder="0 9 * * 1" />
            </div>
          </div>
        </div>
      </div>

      <!-- Webhook 通知配置（飞书机器人） -->
      <div v-if="webhookConfig" class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.webhook.title') }}</h4>

        <div class="space-y-4">
          <div class="flex items-center justify-between">
            <div>
              <label class="font-medium text-gray-900 dark:text-white">{{ t('admin.ops.webhook.enable') }}</label>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.webhook.hint') }}</p>
            </div>
            <Toggle v-model="webhookConfig.enabled" />
          </div>

          <div>
            <label class="input-label">{{ t('admin.ops.webhook.url') }}</label>
            <input
              v-model="webhookConfig.webhook_url"
              type="text"
              class="input font-mono text-sm"
              placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/..."
            />
          </div>

          <div>
            <label class="input-label">{{ t('admin.ops.webhook.secret') }}</label>
            <input
              v-model="webhookSecretInput"
              type="password"
              class="input font-mono text-sm"
              :placeholder="webhookConfig.secret_configured ? t('admin.ops.webhook.secretKeepPlaceholder') : t('admin.ops.webhook.secretPlaceholder')"
            />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.webhook.secretHint') }}</p>
            <label v-if="webhookConfig.secret_configured" class="mt-1 flex items-center gap-2 text-xs text-gray-600 dark:text-gray-400">
              <input
                v-model="webhookClearSecret"
                type="checkbox"
                class="h-3.5 w-3.5"
                :disabled="webhookSecretInput.trim() !== ''"
              />
              {{ t('admin.ops.webhook.clearSecret') }}
            </label>
          </div>

          <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
            <div>
              <label class="input-label">{{ t('admin.ops.webhook.rateLimit') }}</label>
              <input
                v-model.number="webhookConfig.rate_limit_per_minute"
                type="number"
                min="1"
                max="90"
                class="input"
              />
              <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.webhook.rateLimitHint') }}</p>
            </div>
            <div>
              <label class="input-label">{{ t('admin.ops.webhook.atUsers') }}</label>
              <input
                v-model="webhookAtUserIDsInput"
                type="text"
                class="input font-mono text-sm"
                :placeholder="t('admin.ops.webhook.atUsersPlaceholder')"
              />
              <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.webhook.atUsersHint') }}</p>
            </div>
          </div>

          <div>
            <label class="input-label">{{ t('admin.ops.webhook.events') }}</label>
            <div class="space-y-2">
              <div class="flex items-center justify-between">
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.webhook.eventOrderRechargePaid') }}</label>
                <Toggle v-model="webhookConfig.events.order_recharge_paid" />
              </div>
              <div class="flex items-center justify-between">
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.webhook.eventOrderSubscriptionPaid') }}</label>
                <Toggle v-model="webhookConfig.events.order_subscription_paid" />
              </div>
              <div class="flex items-center justify-between">
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.webhook.eventAccountAutoPaused') }}</label>
                <Toggle v-model="webhookConfig.events.account_auto_paused" />
              </div>
              <div class="flex items-center justify-between">
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.webhook.eventApikeyBreakerTripped') }}</label>
                <Toggle v-model="webhookConfig.events.apikey_breaker_tripped" />
              </div>
            </div>
          </div>

          <div class="flex items-center gap-3">
            <button
              class="btn btn-secondary"
              type="button"
              :disabled="sendingWebhookTest"
              @click="sendWebhookTest"
            >
              <svg v-if="sendingWebhookTest" class="mr-1 h-4 w-4 animate-spin" viewBox="0 0 24 24" fill="none">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
              </svg>
              {{ sendingWebhookTest ? t('admin.ops.webhook.testing') : t('admin.ops.webhook.sendTest') }}
            </button>
            <span class="text-xs text-gray-500">{{ t('admin.ops.webhook.sendTestHint') }}</span>
          </div>
        </div>
      </div>

      <!-- 指标阈值配置 -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.settings.metricThresholds') }}</h4>
        <p class="mb-4 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.settings.metricThresholdsHint') }}</p>

        <div class="space-y-4">
          <div>
            <label class="input-label">{{ t('admin.ops.settings.slaMinPercent') }}</label>
            <input
              v-model.number="metricThresholds.sla_percent_min"
              type="number"
              min="0"
              max="100"
              step="0.1"
              class="input"
            />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.slaMinPercentHint') }}</p>
          </div>


          <div>
            <label class="input-label">{{ t('admin.ops.settings.ttftP99MaxMs') }}</label>
            <input
              v-model.number="metricThresholds.ttft_p99_ms_max"
              type="number"
              min="0"
              step="50"
              class="input"
            />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.ttftP99MaxMsHint') }}</p>
          </div>

          <div>
            <label class="input-label">{{ t('admin.ops.settings.requestErrorRateMaxPercent') }}</label>
            <input
              v-model.number="metricThresholds.request_error_rate_percent_max"
              type="number"
              min="0"
              max="100"
              step="0.1"
              class="input"
            />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.requestErrorRateMaxPercentHint') }}</p>
          </div>

          <div>
            <label class="input-label">{{ t('admin.ops.settings.upstreamErrorRateMaxPercent') }}</label>
            <input
              v-model.number="metricThresholds.upstream_error_rate_percent_max"
              type="number"
              min="0"
              max="100"
              step="0.1"
              class="input"
            />
            <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.upstreamErrorRateMaxPercentHint') }}</p>
          </div>
        </div>
      </div>

      <!-- 高级设置 -->
      <details class="rounded-2xl bg-gray-50 dark:bg-dark-700/50">
        <summary class="cursor-pointer p-4 text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.ops.settings.advancedSettings') }}
        </summary>
        <div class="space-y-4 px-4 pb-4">
          <!-- 数据保留策略 -->
          <div class="space-y-3">
            <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.dataRetention') }}</h5>

            <div class="flex items-center justify-between">
              <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.enableCleanup') }}</label>
              <Toggle v-model="advancedSettings.data_retention.cleanup_enabled" />
            </div>

            <div v-if="advancedSettings.data_retention.cleanup_enabled">
              <label class="input-label">{{ t('admin.ops.settings.cleanupSchedule') }}</label>
              <input
                v-model="advancedSettings.data_retention.cleanup_schedule"
                type="text"
                class="input"
                placeholder="0 2 * * *"
              />
              <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.cleanupScheduleHint') }}</p>
            </div>

            <div class="grid grid-cols-1 gap-4 md:grid-cols-3">
              <div>
                <label class="input-label">{{ t('admin.ops.settings.errorLogRetentionDays') }}</label>
                <input
                  v-model.number="advancedSettings.data_retention.error_log_retention_days"
                  type="number"
                  min="0"
                  max="365"
                  class="input"
                />
              </div>
              <div>
                <label class="input-label">{{ t('admin.ops.settings.minuteMetricsRetentionDays') }}</label>
                <input
                  v-model.number="advancedSettings.data_retention.minute_metrics_retention_days"
                  type="number"
                  min="0"
                  max="365"
                  class="input"
                />
              </div>
              <div>
                <label class="input-label">{{ t('admin.ops.settings.hourlyMetricsRetentionDays') }}</label>
                <input
                  v-model.number="advancedSettings.data_retention.hourly_metrics_retention_days"
                  type="number"
                  min="0"
                  max="365"
                  class="input"
                />
              </div>
            </div>
            <p class="text-xs text-gray-500">{{ t('admin.ops.settings.retentionDaysHint') }}</p>
          </div>

          <!-- 预聚合任务 -->
          <div class="space-y-3">
            <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.aggregation') }}</h5>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.enableAggregation') }}</label>
                <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.settings.aggregationHint') }}</p>
              </div>
              <Toggle v-model="advancedSettings.aggregation.aggregation_enabled" />
            </div>
          </div>

          <!-- OpenAI 账号配额自动暂停（全局默认阈值） -->
          <div class="space-y-3">
            <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.openaiQuotaAutoPause') }}</h5>
            <p class="text-xs text-gray-500">{{ t('admin.ops.settings.openaiQuotaAutoPauseHint') }}</p>

            <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
              <div>
                <label class="input-label">{{ t('admin.ops.settings.openaiQuotaAutoPauseDefault5h') }}</label>
                <input
                  v-model.number="quotaAutoPause5hPercent"
                  type="number"
                  min="0"
                  max="100"
                  step="0.1"
                  class="input"
                  data-testid="ops-quota-auto-pause-5h"
                />
              </div>
              <div>
                <label class="input-label">{{ t('admin.ops.settings.openaiQuotaAutoPauseDefault7d') }}</label>
                <input
                  v-model.number="quotaAutoPause7dPercent"
                  type="number"
                  min="0"
                  max="100"
                  step="0.1"
                  class="input"
                  data-testid="ops-quota-auto-pause-7d"
                />
              </div>
            </div>
            <p class="text-xs text-gray-500">{{ t('admin.ops.settings.openaiQuotaAutoPauseThresholdHint') }}</p>
          </div>

          <!-- Error Filtering -->
          <div class="space-y-3">
            <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.errorFiltering') }}</h5>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.ignoreCountTokensErrors') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.ignoreCountTokensErrorsHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.ignore_count_tokens_errors" />
            </div>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.ignoreContextCanceled') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.ignoreContextCanceledHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.ignore_context_canceled" />
            </div>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.ignoreNoAvailableAccounts') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.ignoreNoAvailableAccountsHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.ignore_no_available_accounts" />
            </div>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.ignoreInsufficientBalanceErrors') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.ignoreInsufficientBalanceErrorsHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.ignore_insufficient_balance_errors" />
            </div>
          </div>

          <!-- Auto Refresh -->
          <div class="space-y-3">
            <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.autoRefresh') }}</h5>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.enableAutoRefresh') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.enableAutoRefreshHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.auto_refresh_enabled" />
            </div>

            <div v-if="advancedSettings.auto_refresh_enabled">
              <label class="input-label">{{ t('admin.ops.settings.refreshInterval') }}</label>
              <Select
                v-model="advancedSettings.auto_refresh_interval_seconds"
                :options="[
                  { value: 15, label: t('admin.ops.settings.refreshInterval15s') },
                  { value: 30, label: t('admin.ops.settings.refreshInterval30s') },
                  { value: 60, label: t('admin.ops.settings.refreshInterval60s') }
                ]"
              />
            </div>
          </div>

          <!-- Dashboard Cards -->
          <div class="space-y-3">
            <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.dashboardCards') }}</h5>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.displayAlertEvents') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.displayAlertEventsHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.display_alert_events" />
            </div>

            <div class="flex items-center justify-between">
              <div>
                <label class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.ops.settings.displayOpenAITokenStats') }}</label>
                <p class="mt-1 text-xs text-gray-500">
                  {{ t('admin.ops.settings.displayOpenAITokenStatsHint') }}
                </p>
              </div>
              <Toggle v-model="advancedSettings.display_openai_token_stats" />
            </div>
          </div>
        </div>
      </details>
    </div>

    <template #footer>
      <div class="flex justify-end gap-2">
        <button class="btn btn-secondary" @click="emit('close')">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="loading || !settingsLoaded || saving || !validation.valid" @click="saveAllSettings">
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>
