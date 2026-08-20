<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <div class="flex-1 sm:max-w-64">
            <input
              v-model="searchQuery"
              type="text"
              class="input"
              :placeholder="t('admin.promptRules.searchPlaceholder')"
              @input="handleSearch"
            />
          </div>
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <button
              @click="loadRules"
              :disabled="loading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
            >
              <Icon
                name="refresh"
                size="md"
                :class="{ 'animate-spin': loading }"
              />
            </button>
            <button @click="openCreate" class="btn btn-primary">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t("admin.promptRules.create") }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable
          :columns="columns"
          :data="rules"
          :loading="loading"
          :server-side-sort="true"
          default-sort-key="order"
          default-sort-order="asc"
          @sort="handleSort"
        >
          <template #cell-order="{ value }">
            <span
              class="inline-flex h-6 min-w-6 items-center justify-center rounded bg-gray-100 px-1.5 text-xs font-medium text-gray-700 dark:bg-dark-600 dark:text-gray-300"
            >
              {{ value }}
            </span>
          </template>

          <template #cell-name="{ value, row }">
            <div class="min-w-0">
              <div class="font-medium text-gray-900 dark:text-white">
                {{ value }}
              </div>
              <div
                v-if="row.description"
                class="mt-1 max-w-xs truncate text-xs text-gray-500 dark:text-dark-400"
              >
                {{ row.description }}
              </div>
            </div>
          </template>

          <template #cell-role="{ value }">
            <span class="badge badge-primary">
              {{ t(`admin.promptRules.roleLabel.${value}`) }}
            </span>
          </template>

          <template #cell-action="{ value }">
            <span class="badge badge-gray">
              {{ t(`admin.promptRules.actionLabel.${value}`) }}
            </span>
          </template>

          <template #cell-scope="{ row }">
            <div class="flex flex-col gap-0.5 text-xs">
              <span v-if="row.group_ids.length === 0" class="text-gray-500">{{
                t("admin.promptRules.noGroups")
              }}</span>
              <span v-else class="text-gray-700 dark:text-gray-300">
                {{ getGroupNames(row.group_ids).slice(0, 2).join(", ") }}
                <span v-if="row.group_ids.length > 2" class="text-gray-400"
                  >+{{ row.group_ids.length - 2 }}</span
                >
              </span>
              <span v-if="row.model_ids.length === 0" class="text-gray-500">{{
                t("admin.promptRules.allModels")
              }}</span>
              <span v-else class="text-gray-700 dark:text-gray-300">
                {{ row.model_ids.slice(0, 2).join(", ") }}
                <span v-if="row.model_ids.length > 2" class="text-gray-400"
                  >+{{ row.model_ids.length - 2 }}</span
                >
              </span>
            </div>
          </template>

          <template #cell-enabled="{ row }">
            <button
              @click="toggleEnabled(row)"
              :class="row.enabled ? 'badge-success' : 'badge-gray'"
              class="badge"
            >
              {{ row.enabled ? t("common.enabled") : t("common.disabled") }}
            </button>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center space-x-1">
              <button
                @click="openEdit(row)"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-300"
                :title="t('common.edit')"
              >
                <Icon name="edit" size="sm" />
              </button>
              <button
                @click="confirmDelete(row)"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                :title="t('common.delete')"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.promptRules.noRules')"
              :description="t('admin.promptRules.noRulesDesc')"
              :action-text="t('admin.promptRules.create')"
              @action="openCreate"
            />
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- Create/Edit Dialog -->
    <BaseDialog
      :show="showDialog"
      :title="
        editingRule
          ? t('admin.promptRules.editRule')
          : t('admin.promptRules.createRule')
      "
      width="wide"
      @close="closeDialog"
    >
      <div class="space-y-5">
        <div>
          <label class="input-label">{{
            t("admin.promptRules.fields.name")
          }}</label>
          <input
            v-model="form.name"
            type="text"
            class="input"
            :placeholder="t('admin.promptRules.fields.namePlaceholder')"
          />
        </div>
        <div>
          <label class="input-label">{{
            t("admin.promptRules.fields.description")
          }}</label>
          <input
            v-model="form.description"
            type="text"
            class="input"
            :placeholder="t('admin.promptRules.fields.descriptionPlaceholder')"
          />
        </div>
        <div class="flex items-center justify-between">
          <label class="input-label mb-0">{{
            t("admin.promptRules.fields.enabled")
          }}</label>
          <Toggle v-model="form.enabled" />
        </div>
        <div class="grid grid-cols-3 gap-4">
          <div>
            <label class="input-label">{{
              t("admin.promptRules.fields.role")
            }}</label>
            <Select
              v-model="form.role"
              :options="roleOptions"
              :disabled="form.action === 'replace'"
            />
          </div>
          <div>
            <label class="input-label">{{
              t("admin.promptRules.fields.action")
            }}</label>
            <Select v-model="form.action" :options="actionOptions" />
          </div>
          <div>
            <label class="input-label">{{
              t("admin.promptRules.fields.order")
            }}</label>
            <input v-model.number="form.order" type="number" class="input" />
          </div>
        </div>
        <p class="text-xs text-gray-500 dark:text-dark-400">
          {{ t(`admin.promptRules.fields.actionHelp.${form.role}`) }}
        </p>
        <template v-if="form.action === 'replace'">
          <div>
            <div class="mb-1 flex items-center gap-2">
              <label class="input-label mb-0">{{
                t("admin.promptRules.fields.matchPattern")
              }}</label>
              <label class="flex items-center gap-1 text-xs text-gray-500 dark:text-dark-400">
                <input
                  type="checkbox"
                  :checked="form.match_mode === 'regex'"
                  @change="form.match_mode = ($event.target as HTMLInputElement).checked ? 'regex' : 'plain'"
                  class="h-3.5 w-3.5 rounded border-gray-300"
                />
                {{ t("admin.promptRules.fields.matchModeRegex") }}
              </label>
            </div>
            <textarea
              v-model="form.match_pattern"
              rows="4"
              class="input font-mono text-sm"
              :placeholder="t('admin.promptRules.fields.matchPatternPlaceholder')"
            />
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.promptRules.fields.matchModeHelp") }}
            </p>
          </div>
        </template>
        <div>
          <label class="input-label">{{
            t("admin.promptRules.fields.content")
          }}</label>
          <textarea
            v-model="form.content"
            rows="6"
            class="input font-mono text-sm"
            :placeholder="t('admin.promptRules.fields.contentPlaceholder')"
          />
        </div>

        <!-- Group Selector -->
        <GroupSelector
          v-model="form.groupIds"
          :groups="groups"
          :label="t('admin.promptRules.fields.groupIds')"
        />

        <!-- Model Selector -->
        <div>
          <label class="input-label">{{
            t("admin.promptRules.fields.modelIds")
          }}</label>
          <ModelWhitelistSelector
            v-model="form.modelIds"
            :available-models="availableGroupModels"
            :show-sync-actions="false"
          />
        </div>
      </div>
      <template #footer>
        <div class="flex justify-end gap-2">
          <button @click="closeDialog" class="btn btn-secondary">
            {{ t("common.cancel") }}
          </button>
          <button @click="saveRule" :disabled="saving" class="btn btn-primary">
            {{ saving ? t("common.saving") : t("common.save") }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- Delete Confirm -->
    <ConfirmDialog
      :show="showDeleteConfirm"
      :title="t('admin.promptRules.deleteConfirmTitle')"
      :message="
        t('admin.promptRules.deleteConfirmMessage', {
          name: deletingRule?.name,
        })
      "
      :confirm-text="t('common.delete')"
      :danger="true"
      @confirm="doDelete"
      @cancel="showDeleteConfirm = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { useAppStore } from "@/stores/app";
import { adminAPI } from "@/api/admin";
import promptRuleAPI, { type PromptRule } from "@/api/admin/promptRules";
import { getPersistedPageSize } from "@/composables/usePersistedPageSize";
import type { AdminGroup } from "@/types";
import type { Column } from "@/components/common/types";

import AppLayout from "@/components/layout/AppLayout.vue";
import TablePageLayout from "@/components/layout/TablePageLayout.vue";
import DataTable from "@/components/common/DataTable.vue";
import Pagination from "@/components/common/Pagination.vue";
import EmptyState from "@/components/common/EmptyState.vue";
import BaseDialog from "@/components/common/BaseDialog.vue";
import ConfirmDialog from "@/components/common/ConfirmDialog.vue";
import GroupSelector from "@/components/common/GroupSelector.vue";
import ModelWhitelistSelector from "@/components/account/ModelWhitelistSelector.vue";
import Select from "@/components/common/Select.vue";
import Toggle from "@/components/common/Toggle.vue";
import Icon from "@/components/icons/Icon.vue";

const { t } = useI18n();
const appStore = useAppStore();

const loading = ref(false);
const saving = ref(false);
const rules = ref<PromptRule[]>([]);
const groups = ref<AdminGroup[]>([]);
const showDialog = ref(false);
const showDeleteConfirm = ref(false);
const editingRule = ref<PromptRule | null>(null);
const deletingRule = ref<PromptRule | null>(null);
const availableGroupModels = ref<string[]>([]);
const searchQuery = ref("");
let modelLoadRequestId = 0;

const pagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
  pages: 0,
});

const sortState = reactive({
  sort_by: "order",
  sort_order: "asc" as "asc" | "desc",
});

const form = reactive({
  name: "",
  description: "",
  enabled: true,
  role: "system" as PromptRule["role"],
  action: "prepend" as "prepend" | "append" | "replace",
  order: 0,
  content: "",
  match_pattern: "",
  match_mode: "plain" as "plain" | "regex",
  groupIds: [] as number[],
  modelIds: [] as string[],
});

const roleOptions = computed(() => {
  const options = [
    { value: "system", label: t("admin.promptRules.roleLabel.system") },
    { value: "user", label: t("admin.promptRules.roleLabel.user") },
    { value: "assistant", label: t("admin.promptRules.roleLabel.assistant") },
  ];
  if (form.action === "replace") {
    return options.filter((option) => option.value === "system");
  }
  return options;
});

const actionOptions = computed(() => [
  { value: "prepend", label: t("admin.promptRules.actionLabel.prepend") },
  { value: "append", label: t("admin.promptRules.actionLabel.append") },
  { value: "replace", label: t("admin.promptRules.actionLabel.replace") },
]);

watch(
  () => form.action,
  (action) => {
    if (action === "replace") {
      form.role = "system";
    }
  },
);

const columns = computed<Column[]>(() => [
  { key: "order", label: t("admin.promptRules.columns.order"), sortable: true },
  { key: "name", label: t("admin.promptRules.columns.name"), sortable: true },
  { key: "role", label: t("admin.promptRules.columns.role"), sortable: true },
  {
    key: "action",
    label: t("admin.promptRules.columns.action"),
    sortable: true,
  },
  { key: "scope", label: t("admin.promptRules.columns.scope") },
  {
    key: "enabled",
    label: t("admin.promptRules.columns.status"),
    sortable: true,
  },
  { key: "actions", label: t("admin.promptRules.columns.actions") },
]);

function getGroupNames(ids: number[]): string[] {
  return ids.map(
    (id) => groups.value.find((g) => g.id === id)?.name || `#${id}`,
  );
}

async function loadEffectiveModels(groupIds: number[]) {
  const requestId = ++modelLoadRequestId;
  if (groupIds.length === 0) {
    availableGroupModels.value = [];
    return;
  }

  availableGroupModels.value = [];

  try {
    const modelLists = await Promise.all(
      groupIds.map((groupId) => adminAPI.groups.getEffectiveModels(groupId)),
    );
    if (requestId !== modelLoadRequestId) return;
    availableGroupModels.value = Array.from(new Set(modelLists.flat())).sort();
  } catch {
    if (requestId !== modelLoadRequestId) return;
    availableGroupModels.value = [];
    appStore.showError(t("admin.promptRules.loadModelsFailed"));
  }
}

watch(
  () => [...form.groupIds],
  (groupIds) => loadEffectiveModels(groupIds),
  { immediate: true },
);

function resetForm() {
  form.name = "";
  form.description = "";
  form.enabled = true;
  form.role = "system";
  form.action = "prepend";
  form.order = 0;
  form.content = "";
  form.match_pattern = "";
  form.match_mode = "plain";
  form.groupIds = [];
  form.modelIds = [];
}

let currentController: AbortController | null = null;

async function loadRules() {
  currentController?.abort();
  const requestController = new AbortController();
  currentController = requestController;
  const { signal } = requestController;

  try {
    loading.value = true;
    const response = await promptRuleAPI.list(
      pagination.page,
      pagination.page_size,
      {
        search: searchQuery.value || undefined,
        sort_by: sortState.sort_by,
        sort_order: sortState.sort_order,
      },
      { signal },
    );
    if (signal.aborted || currentController !== requestController) return;

    rules.value = Array.isArray(response.items) ? response.items : [];
    pagination.total = response.total;
    pagination.pages = response.pages;
    pagination.page = response.page;
    pagination.page_size = response.page_size;
  } catch (error: any) {
    if (
      signal.aborted ||
      currentController !== requestController ||
      error?.name === "AbortError" ||
      error?.code === "ERR_CANCELED"
    ) {
      return;
    }
    appStore.showError(t("admin.promptRules.loadFailed"));
  } finally {
    if (currentController === requestController) {
      loading.value = false;
      currentController = null;
    }
  }
}

function handlePageChange(page: number) {
  pagination.page = page;
  loadRules();
}

function handlePageSizeChange(pageSize: number) {
  pagination.page_size = pageSize;
  pagination.page = 1;
  loadRules();
}

function handleSort(key: string, order: "asc" | "desc") {
  sortState.sort_by = key;
  sortState.sort_order = order;
  pagination.page = 1;
  loadRules();
}

let searchDebounceTimer: number | null = null;
function handleSearch() {
  if (searchDebounceTimer) window.clearTimeout(searchDebounceTimer);
  searchDebounceTimer = window.setTimeout(() => {
    pagination.page = 1;
    loadRules();
  }, 300);
}

async function loadGroups() {
  try {
    groups.value = await adminAPI.groups.getAll();
  } catch {
    // silent
  }
}

function openCreate() {
  editingRule.value = null;
  resetForm();
  showDialog.value = true;
}

function openEdit(rule: PromptRule) {
  editingRule.value = rule;
  form.name = rule.name;
  form.description = rule.description || "";
  form.enabled = rule.enabled;
  form.role = rule.role;
  form.action = rule.action;
  form.order = rule.order;
  form.content = rule.content;
  form.match_pattern = rule.match_pattern || "";
  form.match_mode = rule.match_mode || "plain";
  form.groupIds = [...rule.group_ids];
  form.modelIds = [...rule.model_ids];
  showDialog.value = true;
}

function closeDialog() {
  showDialog.value = false;
  editingRule.value = null;
}

async function saveRule() {
  if (!form.name || !form.content) {
    appStore.showError(t("admin.promptRules.nameContentRequired"));
    return;
  }
  saving.value = true;
  try {
    const base = {
      name: form.name,
      description: form.description || null,
      enabled: form.enabled,
      role: form.role,
      action: form.action,
      order: form.order,
      content: form.content,
      group_ids: form.groupIds,
      model_ids: form.modelIds,
      ...(form.action === "replace"
        ? { match_pattern: form.match_pattern, match_mode: form.match_mode }
        : {}),
    };
    if (editingRule.value) {
      await promptRuleAPI.update(editingRule.value.id, base);
    } else {
      await promptRuleAPI.create(base);
    }
    appStore.showSuccess(t("common.success"));
    closeDialog();
    await loadRules();
  } catch {
    appStore.showError(t("admin.promptRules.saveFailed"));
  } finally {
    saving.value = false;
  }
}

async function toggleEnabled(rule: PromptRule) {
  try {
    await promptRuleAPI.toggleEnabled(rule.id, !rule.enabled);
    rule.enabled = !rule.enabled;
  } catch {
    appStore.showError(t("admin.promptRules.saveFailed"));
  }
}

function confirmDelete(rule: PromptRule) {
  deletingRule.value = rule;
  showDeleteConfirm.value = true;
}

async function doDelete() {
  if (!deletingRule.value) return;
  try {
    await promptRuleAPI.delete(deletingRule.value.id);
    appStore.showSuccess(t("common.success"));
    showDeleteConfirm.value = false;
    deletingRule.value = null;
    if (rules.value.length === 1 && pagination.page > 1) {
      pagination.page -= 1;
    }
    await loadRules();
  } catch {
    appStore.showError(t("admin.promptRules.deleteFailed"));
  }
}

onMounted(() => {
  loadRules();
  loadGroups();
});

onUnmounted(() => {
  currentController?.abort();
  if (searchDebounceTimer) window.clearTimeout(searchDebounceTimer);
  modelLoadRequestId += 1;
});
</script>
