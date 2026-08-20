import { apiClient } from "../client";

import type { BasePaginationResponse } from "@/types";

export interface PromptRule {
  id: number;
  name: string;
  description: string | null;
  enabled: boolean;
  order: number;
  role: "system" | "user" | "assistant";
  content: string;
  action: "prepend" | "append" | "replace";
  match_pattern: string;
  match_mode: "plain" | "regex";
  group_ids: number[];
  model_ids: string[];
  created_at: string;
  updated_at: string;
}

export interface CreatePromptRuleRequest {
  name: string;
  description?: string | null;
  enabled?: boolean;
  order?: number;
  role?: "system" | "user" | "assistant";
  content: string;
  action?: "prepend" | "append" | "replace";
  match_pattern?: string;
  match_mode?: "plain" | "regex";
  group_ids?: number[];
  model_ids?: string[];
}

export interface UpdatePromptRuleRequest {
  name?: string;
  description?: string | null;
  enabled?: boolean;
  order?: number;
  role?: "system" | "user" | "assistant";
  content?: string;
  action?: "prepend" | "append" | "replace";
  match_pattern?: string;
  match_mode?: "plain" | "regex";
  group_ids?: number[];
  model_ids?: string[];
}

type PromptRuleListPayload =
  | BasePaginationResponse<PromptRule>
  | PromptRule[]
  | null
  | undefined;

function normalizeListResponse(
  payload: PromptRuleListPayload,
  page: number,
  pageSize: number,
): BasePaginationResponse<PromptRule> {
  if (Array.isArray(payload)) {
    const offset = Math.max(0, page - 1) * pageSize;
    return {
      items: payload.slice(offset, offset + pageSize),
      total: payload.length,
      page,
      page_size: pageSize,
      pages: Math.max(1, Math.ceil(payload.length / pageSize)),
    };
  }

  const response: Partial<BasePaginationResponse<PromptRule>> =
    payload && typeof payload === "object" ? payload : {};
  const items = Array.isArray(response.items) ? response.items : [];
  const total = typeof response.total === "number" ? response.total : items.length;
  const normalizedPageSize =
    typeof response.page_size === "number" && response.page_size > 0
      ? response.page_size
      : pageSize;

  return {
    items,
    total,
    page: typeof response.page === "number" && response.page > 0 ? response.page : page,
    page_size: normalizedPageSize,
    pages:
      typeof response.pages === "number" && response.pages > 0
        ? response.pages
        : Math.max(1, Math.ceil(total / normalizedPageSize)),
  };
}

export async function list(
  page: number = 1,
  pageSize: number = 20,
  filters?: {
    search?: string;
    sort_by?: string;
    sort_order?: "asc" | "desc";
  },
  options?: {
    signal?: AbortSignal;
  },
): Promise<BasePaginationResponse<PromptRule>> {
  const { data } = await apiClient.get<PromptRuleListPayload>(
    "/admin/prompt-rules",
    {
      params: { page, page_size: pageSize, ...filters },
      signal: options?.signal,
    },
  );
  return normalizeListResponse(data, page, pageSize);
}

export async function getById(id: number): Promise<PromptRule> {
  const { data } = await apiClient.get<PromptRule>(`/admin/prompt-rules/${id}`);
  return data;
}

export async function create(
  ruleData: CreatePromptRuleRequest,
): Promise<PromptRule> {
  const { data } = await apiClient.post<PromptRule>(
    "/admin/prompt-rules",
    ruleData,
  );
  return data;
}

export async function update(
  id: number,
  updates: UpdatePromptRuleRequest,
): Promise<PromptRule> {
  const { data } = await apiClient.put<PromptRule>(
    `/admin/prompt-rules/${id}`,
    updates,
  );
  return data;
}

export async function deleteRule(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(
    `/admin/prompt-rules/${id}`,
  );
  return data;
}

export async function toggleEnabled(
  id: number,
  enabled: boolean,
): Promise<PromptRule> {
  return update(id, { enabled });
}

export const promptRuleAPI = {
  list,
  getById,
  create,
  update,
  delete: deleteRule,
  toggleEnabled,
};

export default promptRuleAPI;
