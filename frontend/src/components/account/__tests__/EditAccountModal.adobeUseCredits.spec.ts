import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount, type VueWrapper } from '@vue/test-utils'

const { updateAccountMock, showErrorMock, authIsSimpleMode } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  showErrorMock: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: showErrorMock, showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ get isSimpleMode() { return authIsSimpleMode.value } }) }))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn() })
)
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const account = (platform = 'adobe', type = 'oauth', extra: Record<string, unknown> = {}) => ({
  id: 21, name: 'Adobe', notes: '', platform, type,
  credentials: { cookie: 'ims_sid=x' }, extra,
  proxy_id: null, concurrency: 1, priority: 1, rate_multiplier: 1, status: 'active',
  group_ids: [], expires_at: null, auto_pause_on_expired: false
})

function mountModal(value = account()) {
  return mount(EditAccountModal, {
    props: { show: true, account: value, proxies: [], groups: [] },
    global: { stubs: {
      BaseDialog: BaseDialogStub, Icon: true, ProxySelector: true,
      GroupSelector: true, ModelWhitelistSelector: true
    } }
  })
}

const checkbox = (wrapper: VueWrapper) =>
  wrapper.get('[data-testid="adobe-use-credits-checkbox"]')

const submit = async (wrapper: VueWrapper) => {
  await wrapper.get('form#edit-account-form').trigger('submit.prevent')
}

describe('EditAccountModal Adobe use-credits toggle', () => {
  beforeEach(() => {
    updateAccountMock.mockReset()
    showErrorMock.mockReset()
    updateAccountMock.mockResolvedValue(account())
  })

  it('shows only for Adobe OAuth accounts', () => {
    expect(mountModal().find('[data-testid="adobe-use-credits-toggle"]').exists()).toBe(true)
    // 中转号（apikey）不展示：积分是 Cookie 号的账号属性。
    expect(mountModal(account('adobe', 'apikey')).find('[data-testid="adobe-use-credits-toggle"]').exists()).toBe(false)
    expect(mountModal(account('grok', 'oauth')).find('[data-testid="adobe-use-credits-toggle"]').exists()).toBe(false)
  })

  it('loads the flag from extra and persists true when enabled', async () => {
    const wrapper = mountModal(account('adobe', 'oauth', { adobe_use_credits: true }))
    expect((checkbox(wrapper).element as HTMLInputElement).checked).toBe(true)

    await submit(wrapper)
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalled())
    const payload = updateAccountMock.mock.calls[0][1] as { extra?: Record<string, unknown> }
    expect(payload.extra?.adobe_use_credits).toBe(true)
  })

  it('deletes the key when turned off', async () => {
    const wrapper = mountModal(account('adobe', 'oauth', { adobe_use_credits: true }))
    await checkbox(wrapper).setValue(false)
    await submit(wrapper)
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalled())
    const payload = updateAccountMock.mock.calls[0][1] as { extra?: Record<string, unknown> }
    expect(payload.extra).toBeDefined()
    expect(payload.extra?.adobe_use_credits).toBeUndefined()
  })

  it('keeps extra clean when the account never opted in', async () => {
    const wrapper = mountModal()
    expect((checkbox(wrapper).element as HTMLInputElement).checked).toBe(false)
    await submit(wrapper)
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalled())
    const payload = updateAccountMock.mock.calls[0][1] as { extra?: Record<string, unknown> }
    expect(payload.extra?.adobe_use_credits).toBeUndefined()
  })
})
