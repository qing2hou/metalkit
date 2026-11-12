<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'

import { jobsApi } from '@/api'
import type { Job } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import StatusTag from '@/components/StatusTag.vue'
import { usePollHealth } from '@/composables/usePollHealth'
import { useQuerySync } from '@/composables/useQuerySync'
import { usePagedTable } from '@/composables/usePagedTable'
import { fmtAbsolute, fmtRelative } from '@/lib/format'
import { asRow } from '@/lib/typed'

const list = ref<Job[]>([])
const loading = ref(false)

const filters = reactive({
  search: '',
  status: '' as '' | Job['status'],
  last24h: false,
})

const filtered = computed(() => {
  let rows = list.value
  const q = filters.search.trim().toLowerCase()
  if (q) rows = rows.filter((j) => j.machine_uuid.toLowerCase().includes(q) || j.id.toLowerCase().includes(q))
  if (filters.status) rows = rows.filter((j) => j.status === filters.status)
  if (filters.last24h) {
    const cutoff = Date.now() - 24 * 3600 * 1000
    rows = rows.filter((j) => Date.parse(j.created_at) >= cutoff)
  }
  return rows
})

useQuerySync(
  filters,
  (s): Record<string, string> => ({
    ...(s.search ? { q: String(s.search) } : {}),
    ...(s.status ? { status: String(s.status) } : {}),
    ...(s.last24h ? { h: '24' } : {}),
  }),
  (query) => ({
    search: query.q ?? '',
    status: (query.status as typeof filters.status) ?? '',
    last24h: query.h === '24',
  }),
)

const health = usePollHealth()

async function load(opts: { silent?: boolean } = {}): Promise<void> {
  // silent=后台轮询：不拉遮罩——5s 一次的 mask 会打断正在查看作业的用户
  if (!opts.silent) loading.value = true
  try {
    list.value = await jobsApi.list({ limit: 200 })
    health.noteOk()
  } catch {
    health.noteError()
  } finally {
    loading.value = false
  }
}

// 5s 轮询，页面隐藏暂停
const POLL_MS = 5_000
let timer: ReturnType<typeof setInterval> | null = null

function onVisibility(): void {
  if (document.hidden) {
    if (timer) clearInterval(timer)
    timer = null
  } else {
    void load({ silent: true })
    startTimer()
  }
}

function startTimer(): void {
  if (timer) clearInterval(timer)
  timer = setInterval(() => {
    if (!document.hidden) void load({ silent: true })
  }, POLL_MS)
}

onMounted(() => {
  void load()
  startTimer()
  document.addEventListener('visibilitychange', onVisibility)
})

onUnmounted(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  if (timer) clearInterval(timer)
})

async function cancel(job: Job): Promise<void> {
  try {
    await ElMessageBox.confirm(`取消作业 ${job.id.slice(0, 8)}…？`, '取消作业', { type: 'warning' })
  } catch {
    return
  }
  try {
    await jobsApi.cancel(job.id)
    ElMessage.success('已请求取消')
    await load()
  } catch (err) {
    // 409 = 非法状态转换
    ElMessage.error(`取消失败: ${(err as Error).message}`)
  }
}

async function remove(job: Job): Promise<void> {
  try {
    await ElMessageBox.confirm(`删除作业 ${job.id.slice(0, 8)}… 的记录（含日志）？`, '删除作业', { type: 'warning' })
  } catch {
    return
  }
  try {
    await jobsApi.remove(job.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}

async function purgeFinished(): Promise<void> {
  try {
    await ElMessageBox.confirm('一键清除所有已完成（成功/失败/取消)作业？', '清除已完成', { type: 'warning' })
  } catch {
    return
  }
  try {
    const res = await jobsApi.purge()
    ElMessage.success(`已清除 ${res.deleted} 条作业`)
    await load()
  } catch (err) {
    ElMessage.error(`清除失败: ${(err as Error).message}`)
  }
}

const runningCount = computed(() => list.value.filter((j) => j.status === 'running' || j.status === 'pending').length)

const pagedTable = usePagedTable(filtered)
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <el-alert
        v-if="health.unreachable.value"
        title="无法连接 controller — 数据可能已过期，正在持续重试"
        type="error"
        show-icon
        :closable="false"
        style="margin-bottom: 12px"
      />
      <header class="mk-page-header">
        <h1 class="mk-page-title">作业</h1>
        <p class="mk-page-sub">装机任务的执行记录与实时进度</p>
        <div class="mk-header-actions">
          <span v-if="runningCount" class="mk-subtle">{{ runningCount }} 个进行中 · 5s 自动刷新</span>
          <el-button :icon="Refresh" circle aria-label="刷新" @click="load()"/>
          <el-button type="danger" plain @click="purgeFinished">清除已完成</el-button>
        </div>
      </header>

      <el-card class="mk-card">
        <div class="mk-filters">
          <el-input
            v-model="filters.search"
            placeholder="搜索机器 UUID / 作业 ID"
            clearable
            style="width: 280px"
          />
          <el-radio-group v-model="filters.status">
            <el-radio-button value="">全部</el-radio-button>
            <el-radio-button value="pending">等待</el-radio-button>
            <el-radio-button value="running">运行中</el-radio-button>
            <el-radio-button value="succeeded">成功</el-radio-button>
            <el-radio-button value="failed">失败</el-radio-button>
            <el-radio-button value="cancelled">取消</el-radio-button>
          </el-radio-group>
          <el-checkbox v-model="filters.last24h">最近 24 小时</el-checkbox>
        </div>

        <el-table class="mk-table" v-loading="loading" :data="pagedTable.paged.value" stripe size="small">
          <el-table-column label="状态" width="90">
            <template #default="{ row }"><StatusTag :status="row.status" /></template>
          </el-table-column>
          <el-table-column prop="type" label="类型" width="90" />
          <el-table-column label="机器" min-width="200">
            <template #default="{ row }">
              <router-link :to="`/m/${row.machine_uuid}`" class="mk-link mono" @click.stop>
                {{ row.machine_uuid }}
              </router-link>
            </template>
          </el-table-column>
          <el-table-column label="BMC" min-width="130">
            <template #default="{ row }">
              <span v-if="row.bmc_ip" class="mono">{{ row.bmc_ip }}</span>
              <span v-else class="mk-subtle">—</span>
              <el-tag
                v-if="row.bmc_managed"
                type="success"
                size="small"
                disable-transitions
                style="margin-left: 6px"
              >纳管</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="stage" label="阶段" min-width="150" />
          <el-table-column label="创建" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="结束" width="120">
            <template #default="{ row }">{{ row.finished_at ? fmtRelative(row.finished_at) : '—' }}</template>
          </el-table-column>
          <el-table-column label="错误" min-width="180">
            <template #default="{ row }">
              <span v-if="row.error" class="mk-error" :title="row.error">{{ row.error.slice(0, 60) }}</span>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" min-width="110">
            <template #default="{ row }">
              <router-link :to="`/jobs/${row.id}`"><el-button text size="small">详情</el-button></router-link>
              <el-button
                v-if="row.status === 'pending' || row.status === 'running'"
                text
                size="small"
                type="warning"
                @click="cancel(asRow<Job>(row))"
              >取消</el-button>
              <el-button
                v-else
                text
                size="small"
                type="danger"
                @click="remove(asRow<Job>(row))"
              >删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="没有作业" :image-size="72" />
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
      </el-card>
    </div>
  </AppShell>
</template>

<style scoped>
.mk-filters {
  display: flex;
  gap: 12px;
  align-items: center;
  flex-wrap: wrap;
  margin-bottom: 14px;
}

.mk-header-actions {
  display: flex;
  gap: 10px;
  align-items: center;
}

.mk-link {
  color: var(--el-color-primary);
  text-decoration: none;
}

.mk-error {
  color: var(--el-color-danger);
  font-size: 12px;
}
</style>
