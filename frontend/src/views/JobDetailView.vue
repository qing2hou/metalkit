<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'

import { bindingsApi } from '@/api'
import { jobsApi } from '@/api'
import type { Job, JobLog } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import StatusTag from '@/components/StatusTag.vue'
import { fmtAbsolute, fmtDuration, logLevelClass } from '@/lib/format'

const route = useRoute()
const jobId = computed(() => String(route.params.id ?? ''))

const job = ref<Job | null>(null)
const logs = ref<JobLog[]>([])
const loading = ref(false)
const terminalRef = ref<HTMLElement | null>(null)
const autoScroll = ref(true)

// 增量日志游标：GET /jobs/{id}/logs?since_id=N
let sinceId = 0
let jobTimer: ReturnType<typeof setTimeout> | null = null
let logTimer: ReturnType<typeof setTimeout> | null = null
let tailFetched = false // 终态后是否已补拉过一次日志
let stopped = false

const TERMINAL = ['succeeded', 'failed', 'cancelled']
const isTerminal = computed(() => TERMINAL.includes(job.value?.status ?? ''))

async function loadJob(): Promise<void> {
  if (!jobId.value) return
  try {
    job.value = await jobsApi.get(jobId.value)
    if (isTerminal.value && !tailFetched) {
      // 终态后补拉一次日志收尾，随后停止轮询
      await fetchLogs()
      tailFetched = true
      stopped = true
    }
  } catch {
    // 401 统一跳转；其余错误不打断
  }
}

async function fetchLogs(): Promise<void> {
  if (!jobId.value) return
  try {
    const chunk = await jobsApi.logs(jobId.value, sinceId)
    if (chunk.length) {
      sinceId = chunk[chunk.length - 1].id
      logs.value = logs.value.concat(chunk)
      if (autoScroll.value) {
        await nextTick()
        terminalRef.value?.scrollTo({ top: terminalRef.value.scrollHeight })
      }
    }
  } catch {
    // 日志拉取失败不打断
  }
}

// 自适应轮询：pending/running 1s，终态 5s（停止由 loadJob 的 tail 逻辑决定）
function schedule(): void {
  const delay = isTerminal.value && !tailFetched ? 5_000 : 1_000
  jobTimer = setTimeout(async () => {
    if (stopped) return
    await Promise.all([loadJob(), fetchLogs()])
    if (stopped) return
    schedule()
  }, delay)
}

async function refreshAll(): Promise<void> {
  if (jobTimer) clearTimeout(jobTimer)
  stopped = false
  tailFetched = Boolean(job.value && isTerminal.value)
  loading.value = true
  await Promise.all([loadJob(), fetchLogs()])
  loading.value = false
  schedule()
}

// 路由参数变化（作业间跳转）时重置全部状态
watch(
  jobId,
  () => {
    if (jobTimer) clearTimeout(jobTimer)
    stopped = false
    tailFetched = false
    sinceId = 0
    logs.value = []
    void refreshAll()
  },
  { immediate: true },
)

onUnmounted(() => {
  stopped = true
  if (jobTimer) clearTimeout(jobTimer)
})

const durationMs = computed(() => {
  if (!job.value?.started_at) return null
  const end = job.value.finished_at ? Date.parse(job.value.finished_at) : Date.now()
  return end - Date.parse(job.value.started_at)
})

// 阶段步骤条：stage 字符串 → 步骤序号（pending/download/install/done 之外按运行中处理）
const STAGE_ORDER = ['pending', 'download', 'install', 'done']
const stepsActive = computed(() => {
  if (!job.value) return 0
  if (isTerminal.value) return job.value.status === 'succeeded' ? 4 : 3
  const idx = STAGE_ORDER.indexOf(job.value.stage ?? 'pending')
  return idx < 0 ? 1 : idx
})

// 用户手动滚动查看历史时自动暂停跟随
function onTerminalScroll(): void {
  const el = terminalRef.value
  if (!el) return
  autoScroll.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

async function cancelJob(): Promise<void> {
  if (!job.value) return
  try {
    await ElMessageBox.confirm('取消该作业？', '取消作业', { type: 'warning' })
  } catch {
    return
  }
  try {
    await jobsApi.cancel(job.value.id)
    ElMessage.success('已请求取消')
    await refreshAll()
  } catch (err) {
    // 409 = 非法状态转换
    ElMessage.error(`取消失败: ${(err as Error).message}`)
  }
}

async function retry(): Promise<void> {
  if (!job.value) return
  try {
    await ElMessageBox.confirm(
      '重试会把该机器绑定重新置为 install，由协调器创建新作业。继续？',
      '重试装机',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    // 后端没有直接的 retry endpoint：重试 = 恢复绑定的 desired_state
    await bindingsApi.upsert(job.value.machine_uuid, {
      image_id: job.value.image_id,
      profile_id: job.value.profile_id,
      desired_state: 'install',
    })
    ElMessage.success('已重新下发，新作业将由协调器创建')
  } catch (err) {
    ElMessage.error(`重试失败: ${(err as Error).message}`)
  }
}

async function removeJob(): Promise<void> {
  if (!job.value) return
  try {
    await ElMessageBox.confirm('删除该作业及其日志？', '删除作业', { type: 'warning' })
  } catch {
    return
  }
  try {
    // 仅终态作业可删（后端校验）
    await jobsApi.remove(job.value.id)
    ElMessage.success('已删除')
  } catch (err) {
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}

function exportLogs(): void {
  const text = logs.value.map((l) => `${l.ts} [${l.level}] ${l.message}`).join('\n')
  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `job-${jobId.value.slice(0, 8)}.log`
  a.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <AppShell>
    <div v-loading="loading" class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">作业详情</h1>
        <div class="mk-header-right">
          <el-button :icon="Refresh" circle aria-label="刷新" @click="refreshAll" />
          <el-button v-if="job && !isTerminal" type="warning" @click="cancelJob">取消作业</el-button>
          <el-button v-if="job && isTerminal" @click="retry">重试</el-button>
          <el-button v-if="job && isTerminal" type="danger" plain @click="removeJob">删除</el-button>
        </div>
      </header>

      <el-card v-if="job" class="mk-card">
        <div class="mk-job-head">
          <StatusTag :status="job.status" />
          <span class="mono mk-subtle">{{ job.id }}</span>
          <el-tag size="small" disable-transitions>{{ job.type }}</el-tag>
          <span v-if="durationMs !== null" class="mk-muted">耗时 {{ fmtDuration(durationMs) }}</span>
        </div>
        <dl class="mk-kv-grid" style="margin-top: 12px">
          <div class="mk-kv">
            <dt>机器</dt>
            <dd>
              <router-link :to="`/m/${job.machine_uuid}`" class="mk-link mono">
                {{ job.machine_uuid }}
              </router-link>
            </dd>
          </div>
          <div class="mk-kv"><dt>创建</dt><dd>{{ fmtAbsolute(job.created_at) }}</dd></div>
          <div class="mk-kv"><dt>开始</dt><dd>{{ fmtAbsolute(job.started_at) }}</dd></div>
          <div class="mk-kv"><dt>结束</dt><dd>{{ fmtAbsolute(job.finished_at) }}</dd></div>
          <div class="mk-kv"><dt>阶段</dt><dd>{{ job.stage || '—' }}</dd></div>
          <div class="mk-kv"><dt>创建者</dt><dd>{{ job.created_by || '—' }}</dd></div>
        </dl>
        <el-alert
          v-if="job.error"
          :title="job.error"
          type="error"
          show-icon
          :closable="false"
          style="margin-top: 12px"
        />
      </el-card>

      <el-card v-if="job" class="mk-card">
        <template #header><span>阶段</span></template>
        <el-steps
          :active="stepsActive"
          align-center
          finish-status="success"
          :process-status="job.status === 'failed' ? 'error' : 'process'"
        >
          <el-step title="等待" description="PXE / Agent 领取" />
          <el-step title="下载" description="拉取镜像" />
          <el-step title="安装" description="写盘 / 配置" />
          <el-step title="完成" :description="job.status" />
        </el-steps>
      </el-card>

      <el-card class="mk-card">
        <template #header>
          <div class="mk-card-header">
            <span>日志（{{ logs.length }} 行）</span>
            <div>
              <el-checkbox v-model="autoScroll" size="small">自动滚动</el-checkbox>
              <el-button size="small" @click="exportLogs">导出</el-button>
            </div>
          </div>
        </template>
        <div ref="terminalRef" class="mk-terminal" @scroll="onTerminalScroll">
          <div v-for="l in logs" :key="l.id" :class="logLevelClass(l.level)">
            <span class="log-ts">{{ fmtAbsolute(l.ts) }}</span> {{ l.message }}
          </div>
          <div v-if="!logs.length && !loading" class="log-debug">
            （暂无日志，作业开始后 Agent 将逐行回传）
          </div>
        </div>
      </el-card>
    </div>
  </AppShell>
</template>

<style scoped>
.mk-header-right {
  display: flex;
  gap: 10px;
  align-items: center;
}

.mk-job-head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.mk-card-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.mk-link {
  color: var(--el-color-primary);
  text-decoration: none;
}
</style>
