<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import type { Ref } from 'vue'

import { jobsApi } from '@/api'
import type { Job } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import StatusTag from '@/components/StatusTag.vue'
import { usePollHealth } from '@/composables/usePollHealth'
import { useQuerySync } from '@/composables/useQuerySync'
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

async function load(): Promise<void> {
  loading.value = true
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
    void load()
    startTimer()
  }
}

function startTimer(): void {
  if (timer) clearInterval(timer)
  timer = setInterval(() => {
    if (!document.hidden) void load()
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
        <h1 class="mk-page-title">装机作业</h1>
        <div class="mk-header-right">
          <span v-if="runningCount" class="mk-subtle">{{ runningCount }} 个进行中 · 5s 自动刷新</span>
          <el-button :icon="Refresh" circle aria-label="刷新" @click="load" />
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

        <el-table v-loading="loading" :data="filtered" stripe size="small">
          <el-table-column label="状态" width="90">
            <template #default="{ row }"><StatusTag :status="row.status" /></template>
          </el-table-column>
          <el-table-column prop="type" label="类型" width="90" />
          <el-table-column label="机器" min-width="200">
            <template #default="{ row }">
              <router-link :to="`/m/${row.machine_uuid}`" class="mk-link mono" @click.stop>
                {{ row.machine_uuid.slice(0, 13) }}…
              </router-link>
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
          <el-table-column label="操作" width="130" fixed="right">
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

.mk-header-right {
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
