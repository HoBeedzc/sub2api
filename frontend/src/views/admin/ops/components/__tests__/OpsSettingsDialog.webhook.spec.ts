import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
enableAutoUnmount(afterEach)

import OpsSettingsDialog from '../OpsSettingsDialog.vue'

const defaultWebhookConfig = {
  enabled: false,
  provider: 'feishu',
  webhook_url: 'https://open.feishu.cn/open-apis/bot/v2/hook/x',
  secret_configured: true,
  rate_limit_per_minute: 50,
  at_user_ids: [],
  events: { order_recharge_paid: true, order_subscription_paid: true, account_auto_paused: true, apikey_breaker_tripped: true },
}

const { opsAPI, showError, showSuccess } = vi.hoisted(() => ({
  opsAPI: {
    getAlertRuntimeSettings: vi.fn(), getEmailNotificationConfig: vi.fn(),
    getAdvancedSettings: vi.fn(), getMetricThresholds: vi.fn(),
    updateAlertRuntimeSettings: vi.fn(), updateEmailNotificationConfig: vi.fn(),
    updateAdvancedSettings: vi.fn(), updateMetricThresholds: vi.fn(),
    getWebhookNotificationConfig: vi.fn(), updateWebhookNotificationConfig: vi.fn(),
    testWebhookNotification: vi.fn(),
  },
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))
vi.mock('@/api/admin/ops', () => ({ opsAPI }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess }) }))

async function openDialog() {
  const wrapper = shallowMount(OpsSettingsDialog, {
    props: { show: false },
    global: { stubs: { BaseDialog: { template: '<div><slot name="footer" /></div>' } } },
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

type View = {
  saveAllSettings: () => Promise<void>
  sendWebhookTest: () => Promise<void>
}

describe('ops settings webhook section', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    opsAPI.getAlertRuntimeSettings.mockResolvedValue({ evaluation_interval_seconds: 30 })
    opsAPI.getEmailNotificationConfig.mockResolvedValue({ alert: { enabled: false }, report: { enabled: false } })
    opsAPI.getAdvancedSettings.mockResolvedValue({ data_retention: { error_log_retention_days: 7, minute_metrics_retention_days: 7, hourly_metrics_retention_days: 7 } })
    opsAPI.getMetricThresholds.mockResolvedValue({ sla_percent_min: 98 })
    opsAPI.getWebhookNotificationConfig.mockResolvedValue({ ...defaultWebhookConfig })
  })
  afterEach(() => { vi.restoreAllMocks() })

  it('loads webhook config with the other settings', async () => {
    await openDialog()
    expect(opsAPI.getWebhookNotificationConfig).toHaveBeenCalledTimes(1)
  })

  it('saves webhook config including a newly typed secret, resetting it afterwards', async () => {
    const wrapper = await openDialog()
    const vm = wrapper.vm as unknown as View
    await vm.saveAllSettings()
    expect(opsAPI.updateWebhookNotificationConfig).toHaveBeenCalledWith(expect.objectContaining({
      enabled: false,
      webhook_url: defaultWebhookConfig.webhook_url,
      secret: '',
      clear_secret: false,
      rate_limit_per_minute: 50,
    }))
  })

  it('sends the test request even when the draft URL is empty (backend falls back)', async () => {
    opsAPI.getWebhookNotificationConfig.mockResolvedValue({ ...defaultWebhookConfig, webhook_url: '' })
    opsAPI.testWebhookNotification.mockResolvedValue({ message: 'ok' })
    const wrapper = await openDialog()
    await (wrapper.vm as unknown as View).sendWebhookTest()
    await flushPromises()
    expect(opsAPI.testWebhookNotification).toHaveBeenCalledWith(expect.objectContaining({
      webhook_url: '',
      ignore_stored_secret: false,
    }))
  })

  it('blocks save when enabled without a webhook url', async () => {
    opsAPI.getWebhookNotificationConfig.mockResolvedValue({ ...defaultWebhookConfig, enabled: true, webhook_url: '' })
    const wrapper = await openDialog()
    const vm = wrapper.vm as unknown as View
    await vm.saveAllSettings()
    expect(opsAPI.updateWebhookNotificationConfig).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it('blocks save when enabled with an out-of-range rate limit', async () => {
    opsAPI.getWebhookNotificationConfig.mockResolvedValue({ ...defaultWebhookConfig, enabled: true, rate_limit_per_minute: 0 })
    const wrapper = await openDialog()
    const vm = wrapper.vm as unknown as View
    await vm.saveAllSettings()
    expect(opsAPI.updateWebhookNotificationConfig).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.ops.webhook.validation.rateLimitRange')
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it('falls back to a safe default config when the webhook endpoint is missing (mixed-version deploy)', async () => {
    opsAPI.getWebhookNotificationConfig.mockRejectedValue({ response: { status: 404 } })
    const wrapper = await openDialog()
    // 其余配置照常加载，弹窗不报错
    expect(opsAPI.getAlertRuntimeSettings).toHaveBeenCalledTimes(1)
    expect(opsAPI.getEmailNotificationConfig).toHaveBeenCalledTimes(1)
    expect(showError).not.toHaveBeenCalled()
    // 默认态可绑定并可保存
    const vm = wrapper.vm as unknown as View
    await vm.saveAllSettings()
    expect(opsAPI.updateWebhookNotificationConfig).toHaveBeenCalledWith(expect.objectContaining({
      enabled: false,
      rate_limit_per_minute: 50
    }))
  })

  it('isolates a webhook save failure so the other settings still save', async () => {
    opsAPI.updateWebhookNotificationConfig.mockRejectedValue({ response: { status: 404, data: { message: 'not found' } } })
    const wrapper = await openDialog()
    const vm = wrapper.vm as unknown as View
    await vm.saveAllSettings()
    expect(opsAPI.updateAlertRuntimeSettings).toHaveBeenCalled()
    expect(opsAPI.updateMetricThresholds).toHaveBeenCalled()
    expect(showSuccess).toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.ops.webhook.saveFailed')
    expect(wrapper.emitted('saved')).toBeDefined()
  })

  it('sends a test message with the current form values and toasts success', async () => {
    opsAPI.testWebhookNotification.mockResolvedValue({ message: 'ok' })
    const wrapper = await openDialog()
    await (wrapper.vm as unknown as View).sendWebhookTest()
    await flushPromises()
    expect(opsAPI.testWebhookNotification).toHaveBeenCalledWith(expect.objectContaining({
      webhook_url: defaultWebhookConfig.webhook_url,
      secret: '',
    }))
    expect(showSuccess).toHaveBeenCalledWith('ok')
  })

  it('toasts an error when the test send fails', async () => {
    opsAPI.testWebhookNotification.mockRejectedValue({ response: { data: { message: 'sign match fail' } } })
    const wrapper = await openDialog()
    await (wrapper.vm as unknown as View).sendWebhookTest()
    await flushPromises()
    expect(showError).toHaveBeenCalledWith('sign match fail')
  })
})
