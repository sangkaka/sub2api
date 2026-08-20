import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const { createRule, getEffectiveModels, getGroups, listRules, updateRule } = vi.hoisted(() => ({
  createRule: vi.fn(),
  getEffectiveModels: vi.fn(),
  getGroups: vi.fn(),
  listRules: vi.fn(),
  updateRule: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: {
      getEffectiveModels,
      getAll: getGroups
    }
  }
}))

vi.mock('@/api/admin/promptRules', () => ({
  default: {
    list: listRules,
    create: createRule,
    update: updateRule,
    delete: vi.fn(),
    toggleEnabled: vi.fn()
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import PromptRulesView from '../PromptRulesView.vue'
import ModelWhitelistSelector from '@/components/account/ModelWhitelistSelector.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import Toggle from '@/components/common/Toggle.vue'
import zhPromptRules from '@/i18n/locales/zh/admin/promptRules'
import enPromptRules from '@/i18n/locales/en/admin/promptRules'

const SlotStub = defineComponent({
  setup(_, { slots }) {
    return () => h('div', [
      slots.default?.(),
      slots.filters?.(),
      slots.table?.(),
      slots.pagination?.(),
      slots.footer?.()
    ])
  }
})

const BaseDialogStub = defineComponent({
  props: { show: Boolean },
  setup(props, { slots }) {
    return () => props.show ? h('section', [slots.default?.(), slots.footer?.()]) : null
  }
})

function mountView() {
  return mount(PromptRulesView, {
    global: {
      stubs: {
        AppLayout: SlotStub,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        GroupSelector: true,
        Icon: true,
        ModelWhitelistSelector,
        TablePageLayout: SlotStub,
        Toggle
      }
    }
  })
}

const disabledRule = {
  id: 1,
  name: 'Disabled rule',
  description: null,
  enabled: false,
  order: 0,
  role: 'system' as const,
  content: 'Prompt content',
  action: 'prepend' as const,
  group_ids: [],
  model_ids: ['claude-sonnet-4-6'],
  created_at: '2026-07-17T00:00:00Z',
  updated_at: '2026-07-17T00:00:00Z'
}

function paginatedRules(items: typeof disabledRule[], overrides: Record<string, number> = {}) {
  return {
    items,
    total: items.length,
    page: 1,
    page_size: 20,
    pages: 1,
    ...overrides
  }
}

describe('PromptRulesView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    localStorage.clear()
    listRules.mockResolvedValue(paginatedRules([]))
    getGroups.mockResolvedValue([])
    getEffectiveModels.mockResolvedValue([])
    createRule.mockResolvedValue({})
    updateRule.mockResolvedValue({})
  })

  it('uses neutral prompt rule copy without injection terminology', () => {
    expect(zhPromptRules.promptRules.title).toBe('提示词规则')
    expect(enPromptRules.promptRules.title).toBe('Prompt Rules')
    expect(JSON.stringify(zhPromptRules)).not.toContain('注入')
    expect(JSON.stringify(enPromptRules).toLowerCase()).not.toContain('inject')
  })

  it('uses the model restriction copy and removes the old empty-selection hints', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.promptRules.create'))
    await createButton!.trigger('click')

    expect(zhPromptRules.promptRules.fields.modelIds).toBe('模型限制（可选）')
    expect(enPromptRules.promptRules.fields.modelIds).toBe('Model Restriction (Optional)')
    expect(zhPromptRules.promptRules.fields.groupIds).toBe('适用分组')
    expect(enPromptRules.promptRules.fields.groupIds).toBe('Target Groups')
    expect(wrapper.text()).not.toContain('admin.promptRules.allGroupsHint')
    expect(wrapper.text()).not.toContain('admin.promptRules.allModelsHint')
  })

  it('configures the shared selector without sync actions', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.promptRules.create'))
    await createButton!.trigger('click')

    const selector = wrapper.getComponent(ModelWhitelistSelector)
    expect(selector.props('showSyncActions')).toBe(false)
  })

  it('loads model options from the selected groups', async () => {
    listRules.mockResolvedValue(paginatedRules([{ ...disabledRule, group_ids: [7, 8] }]))
    getGroups.mockResolvedValue([
      { id: 7, name: 'OpenAI group A', platform: 'openai' },
      { id: 8, name: 'OpenAI group B', platform: 'openai' }
    ])
    getEffectiveModels.mockImplementation((groupId: number) => Promise.resolve(
      groupId === 7
        ? ['shared-model', 'group-a-model']
        : ['group-b-model', 'shared-model']
    ))
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.find('button[title="common.edit"]')
    await editButton!.trigger('click')
    await flushPromises()

    expect(getEffectiveModels).toHaveBeenCalledWith(7)
    expect(getEffectiveModels).toHaveBeenCalledWith(8)
    expect(wrapper.getComponent(ModelWhitelistSelector).props('availableModels')).toEqual([
      'group-a-model',
      'group-b-model',
      'shared-model'
    ])
  })

  it('uses the account form label typography and spacing', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.promptRules.create'))
    await createButton!.trigger('click')

    expect(wrapper.find('.space-y-5').exists()).toBe(true)
    expect(wrapper.findAll('label.label')).toHaveLength(0)
    expect(wrapper.findAll('label.input-label').length).toBeGreaterThanOrEqual(7)
  })

  it('creates enabled rules by default and submits the enabled field', async () => {
    const wrapper = mountView()
    await flushPromises()

    const createButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('admin.promptRules.create'))
    await createButton!.trigger('click')

    const toggle = wrapper.getComponent(Toggle)
    expect(toggle.props('modelValue')).toBe(true)
    await toggle.trigger('click')

    const textInputs = wrapper.findAll('section input[type="text"]')
    await textInputs[0].setValue('Paused rule')
    await wrapper.get('textarea').setValue('Prompt content')

    const saveButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('common.save'))
    await saveButton!.trigger('click')
    await flushPromises()

    expect(createRule).toHaveBeenCalledWith(expect.objectContaining({
      enabled: false,
      group_ids: [],
      model_ids: []
    }))
  })

  it('loads disabled state for editing and labels empty groups as unselected', async () => {
    listRules.mockResolvedValue(paginatedRules([disabledRule]))
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.text()).toContain('admin.promptRules.noGroups')
    expect(wrapper.text()).not.toContain('admin.promptRules.allGroups')

    const editButton = wrapper.find('button[title="common.edit"]')
    await editButton!.trigger('click')

    expect(wrapper.getComponent(Toggle).props('modelValue')).toBe(false)
    expect(wrapper.getComponent(ModelWhitelistSelector).props('modelValue')).toEqual([
      'claude-sonnet-4-6'
    ])

    const saveButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('common.save'))
    await saveButton!.trigger('click')
    await flushPromises()

    expect(updateRule).toHaveBeenCalledWith(1, expect.objectContaining({ enabled: false }))
  })

  it('keeps the shared table visible when the result is empty', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.getComponent(DataTable).props('data')).toEqual([])
    expect(wrapper.find('table').exists()).toBe(true)
    expect(wrapper.find('thead').text()).toContain('admin.promptRules.columns.name')
    expect(wrapper.text()).toContain('admin.promptRules.noRules')
  })

  it('requests server-side sorting and pagination', async () => {
    listRules.mockResolvedValue(paginatedRules([disabledRule], { total: 40, pages: 2 }))
    const wrapper = mountView()
    await flushPromises()
    listRules.mockClear()

    wrapper.getComponent(DataTable).vm.$emit('sort', 'name', 'desc')
    await flushPromises()
    expect(listRules).toHaveBeenLastCalledWith(
      1,
      20,
      expect.objectContaining({ sort_by: 'name', sort_order: 'desc' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )

    wrapper.getComponent(Pagination).vm.$emit('update:page', 2)
    await flushPromises()
    expect(listRules).toHaveBeenLastCalledWith(
      2,
      20,
      expect.objectContaining({ sort_by: 'name', sort_order: 'desc' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
  })

  it('debounces search and resets the page', async () => {
    vi.useFakeTimers()
    try {
      const wrapper = mountView()
      await flushPromises()
      listRules.mockClear()

      await wrapper.get('input[placeholder="admin.promptRules.searchPlaceholder"]').setValue('rule')
      await vi.advanceTimersByTimeAsync(300)
      await flushPromises()

      expect(listRules).toHaveBeenCalledTimes(1)
      expect(listRules).toHaveBeenCalledWith(
        1,
        20,
        expect.objectContaining({ search: 'rule', sort_by: 'order', sort_order: 'asc' }),
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      )
    } finally {
      vi.useRealTimers()
    }
  })
})
