import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({
  get: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { get }
}))

import { list, type PromptRule } from '@/api/admin/promptRules'

const rule: PromptRule = {
  id: 1,
  name: 'System rule',
  description: null,
  enabled: true,
  order: 0,
  role: 'system',
  content: 'Prompt content',
  action: 'prepend',
  group_ids: [],
  model_ids: [],
  created_at: '2026-07-18T00:00:00Z',
  updated_at: '2026-07-18T00:00:00Z'
}

describe('admin prompt rules API', () => {
  beforeEach(() => {
    get.mockReset()
  })

  it('normalizes the legacy array response into pagination data', async () => {
    get.mockResolvedValue({ data: [rule] })

    await expect(list(1, 10)).resolves.toEqual({
      items: [rule],
      total: 1,
      page: 1,
      page_size: 10,
      pages: 1
    })
  })

  it('normalizes an incomplete response to an empty list', async () => {
    get.mockResolvedValue({ data: {} })

    await expect(list(1, 20)).resolves.toEqual({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      pages: 1
    })
  })

  it('preserves a valid paginated response', async () => {
    const response = {
      items: [rule],
      total: 21,
      page: 2,
      page_size: 10,
      pages: 3
    }
    get.mockResolvedValue({ data: response })

    await expect(list(2, 10)).resolves.toEqual(response)
  })
})
