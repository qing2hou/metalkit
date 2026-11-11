<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'

import { auditApi } from '@/api'
import type { AuditEvent } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { useQuerySync } from '@/composables/useQuerySync'
import { usePagedTable } from '@/composables/usePagedTable'
import { fmtAbsolute } from '@/lib/format'
import { asRow } from '@/lib/typed'

const list = ref<AuditEvent[]>([])
const loading = ref(false)

const filters = reactive({
  actor: '',
  action: '',
})

useQuerySync(
  filters,
  (s): Record<string, string> => ({
    ...(s.actor ? { actor: String(s.actor) } : {}),
    ...(s.action ? { action: String(s.action) } : {}),
  }),
  (q) => ({ actor: q.actor ?? '', action: q.action ?? '' }),
)

async function load(): Promise<void> {
  loading.value = true
  try {
    list.value = await auditApi.list({
      actor: filters.actor.trim() || undefined,
      action: filters.action.trim() || undefined,
      limit: 200,
    })
  } catch (err) {
    loadError.value = `加载审计记录失败: ${(err as Error).message}`
  } finally {
    loading.value = false
  }
}

const loadError = ref('')

onMounted(load)

function outcomeType(outcome: string): 'success' | 'danger' {
  return outcome === 'ok' ? 'success' : 'danger'
}

const actionFilters = [
  '',
  'PUT /api/v1/bindings',
  'binding.password_view',
  'POST /api/v1/images/uploads',
  'DELETE /api/v1/images',
  'DELETE /api/v1/machines',
  'POST /api/v1/bmc',
]

// 删除单条审计记录（Admin 操作；删除会新记一条审计）
async function remove(row: AuditEvent): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `删除这条审计记录（#${row.id} ${row.action}）？`,
      '删除审计记录',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await auditApi.remove(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}

// 清空全部审计记录（两步确认）
async function purgeAll(): Promise<void> {
  try {
    await ElMessageBox.confirm(`清空全部 ${pagedTable.total.value} 条审计记录？此操作不可恢复。`, '清空审计', {
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    const { value } = await ElMessageBox.prompt('请输入「清空」两个字以确认执行', '最终确认', {
      confirmButtonText: '执行清空',
      cancelButtonText: '取消',
      inputValidator: (v: string) => v.trim() === '清空' || '输入不正确，请输入「清空」',
    })
    if (value.trim() !== '清空') return
  } catch {
    return
  }
  try {
    const r = await auditApi.purge()
    ElMessage.success(`已清空 ${r.deleted} 条记录`)
    await load()
  } catch (err) {
    ElMessage.error(`清空失败: ${(err as Error).message}`)
  }
}

const pagedTable = usePagedTable(list)
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">审计</h1>
        <div class="mk-header-actions">
          <el-button type="danger" plain :disabled="!pagedTable.total.value" @click="purgeAll">清空</el-button>
          <el-button :icon="Refresh" circle aria-label="刷新" @click="load" />
        </div>
      </header>

      <el-alert v-if="loadError" :title="loadError" type="error" show-icon :closable="false" style="margin-bottom: 12px" />

      <el-card class="mk-card">
        <div class="mk-filters">
          <el-input
            v-model="filters.actor"
            placeholder="按操作者过滤（如 alice）"
            clearable
            style="width: 220px"
            @change="load"
          />
          <el-select
            v-model="filters.action"
            placeholder="按动作过滤"
            clearable
            filterable
            allow-create
            style="width: 280px"
            @change="load"
          >
            <el-option v-for="a in actionFilters" :key="a" :label="a || '全部动作'" :value="a" />
          </el-select>
        </div>

        <el-table class="mk-table" v-loading="loading" :data="pagedTable.paged.value" stripe size="small">
          <el-table-column label="时间" width="170">
            <template #default="{ row }">{{ fmtAbsolute(row.ts) }}</template>
          </el-table-column>
          <el-table-column prop="actor" label="操作者" width="120">
            <template #default="{ row }"><span class="mono">{{ row.actor }}</span></template>
          </el-table-column>
          <el-table-column prop="action" label="动作" min-width="260">
            <template #default="{ row }"><span class="mono">{{ row.action }}</span></template>
          </el-table-column>
          <el-table-column prop="target" label="目标" min-width="160">
            <template #default="{ row }">
              <span v-if="row.target" class="mono">{{ row.target }}</span>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="结果" width="90">
            <template #default="{ row }">
              <el-tag :type="outcomeType(row.outcome)" size="small" disable-transitions>
                {{ row.outcome === 'ok' ? '成功' : '失败' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="详情" min-width="200">
            <template #default="{ row }">
              <span v-if="row.details && Object.keys(row.details).length" class="mono mk-subtle">
                {{ row.details }}
              </span>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" min-width="70">
            <template #default="{ row }">
              <el-button text size="small" type="danger" @click.stop="remove(asRow<AuditEvent>(row))">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="暂无审计记录" :image-size="72" />
          </template>
        </el-table>
        <el-pagination
          v-model:current-page="pagedTable.currentPage.value"
          v-model:page-size="pagedTable.pageSize.value"
          :page-sizes="[20, 50, 100]"
          :total="pagedTable.total.value"
          layout="total, sizes, prev, pager, next, jumper"
          style="margin-top: 16px; justify-content: flex-end"
        />
        <div class="mk-subtle" style="margin-top: 8px">
          记录所有变更类 API 调用（含失败）与敏感操作（如装机密码查看）。只读，追加写。
        </div>
      </el-card>
    </div>
  </AppShell>
</template>

<style scoped>
.mk-filters {
  display: flex;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
</style>
