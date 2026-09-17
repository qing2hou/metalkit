<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'

import { bindingsApi, imagesApi, machinesApi, profilesApi, subnetsApi } from '@/api'
import type { Binding, Image, MachineSummary, Profile, Report, Subnet } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import CopyableText from '@/components/CopyableText.vue'
import InstallDialog from '@/components/InstallDialog.vue'
import StatusTag from '@/components/StatusTag.vue'
import { usePollHealth } from '@/composables/usePollHealth'
import { useQuerySync } from '@/composables/useQuerySync'
import { fmtRelative } from '@/lib/format'
import { asRow } from '@/lib/typed'

const router = useRouter()
const list = ref<MachineSummary[]>([])
const loading = ref(false)

// 过滤状态（与 URL query 双向同步，对应旧版 list-filter.js）
const filters = reactive({
  search: '',
  status: '' as '' | 'online' | 'offline',
  managed: '' as '' | 'yes' | 'no',
})

const filtered = computed(() => {
  let rows = list.value
  const q = filters.search.trim().toLowerCase()
  if (q) {
    rows = rows.filter(
      (m) =>
        m.uuid.toLowerCase().includes(q) ||
        (m.serial ?? '').toLowerCase().includes(q) ||
        (m.product_name ?? '').toLowerCase().includes(q) ||
        (m.manufacturer ?? '').toLowerCase().includes(q) ||
        (m.bmc_ip ?? '').includes(q),
    )
  }
  if (filters.status) rows = rows.filter((m) => (m.status ?? 'unknown') === filters.status)
  if (filters.managed) {
    const want = filters.managed === 'yes'
    rows = rows.filter((m) => Boolean(m.bmc_managed) === want)
  }
  return rows
})

useQuerySync(
  filters,
  (s): Record<string, string> => ({
    ...(s.search ? { q: String(s.search) } : {}),
    ...(s.status ? { status: String(s.status) } : {}),
    ...(s.managed ? { managed: String(s.managed) } : {}),
  }),
  (query) => ({
    search: query.q ?? '',
    status: (query.status as typeof filters.status) ?? '',
    managed: (query.managed as typeof filters.managed) ?? '',
  }),
)

const health = usePollHealth()

async function load(): Promise<void> {
  loading.value = true
  try {
    list.value = await machinesApi.list()
    health.noteOk()
  } catch {
    // 401 已在 client 层跳转；其余错误连续 2 次后显示常驻横幅
    health.noteError()
  } finally {
    loading.value = false
  }
}

// ---- 列表页直达装机弹窗：点「装机」按需拉取该机器的配置数据 ----
const installVisible = ref(false)
const installUuid = ref('')
const installReport = ref<Report | null>(null)
const installImages = ref<Image[]>([])
const installProfiles = ref<Profile[]>([])
const installSubnets = ref<Subnet[]>([])
const installBinding = ref<Binding | null>(null)
const installLoading = ref(false)

async function openInstall(row: MachineSummary): Promise<void> {
  installLoading.value = true
  installUuid.value = row.uuid
  try {
    const [report, images, profiles, subnets, binding] = await Promise.all([
      machinesApi.get(row.uuid).catch(() => null),
      imagesApi.list(),
      profilesApi.list(),
      subnetsApi.list(),
      bindingsApi.get(row.uuid).catch(() => null),
    ])
    installReport.value = report
    installImages.value = images
    installProfiles.value = profiles
    installSubnets.value = subnets
    installBinding.value = binding
    installVisible.value = true
  } catch (err) {
    ElMessage.error(`加载装机数据失败: ${(err as Error).message}`)
  } finally {
    installLoading.value = false
  }
}

const POLL_LIST_MS = 30_000
const remainingSec = ref(30)
let timer: ReturnType<typeof setInterval> | null = null
let pollTimer: ReturnType<typeof setInterval> | null = null

function stopTimers(): void {
  if (timer) clearInterval(timer)
  if (pollTimer) clearInterval(pollTimer)
  timer = null
  pollTimer = null
}

function onVisibility(): void {
  if (document.hidden) {
    stopTimers()
  } else {
    startPolling()
    void load()
  }
}

function startPolling(): void {
  stopTimers()
  remainingSec.value = POLL_LIST_MS / 1000
  timer = setInterval(() => {
    if (remainingSec.value > 0) remainingSec.value--
  }, 1000)
  pollTimer = setInterval(() => {
    void load()
  }, POLL_LIST_MS)
}

onMounted(() => {
  void load()
  startPolling()
  document.addEventListener('visibilitychange', onVisibility)
})

onUnmounted(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  stopTimers()
})

async function refreshNow(): Promise<void> {
  await load()
  startPolling()
}

function openDetail(row: MachineSummary): void {
  router.push(`/m/${row.uuid}`)
}

async function remove(row: MachineSummary): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `删除机器 ${row.serial || row.uuid} 的全部上报与绑定数据？此操作不可恢复。`,
      '删除机器',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await machinesApi.remove(row.uuid)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    // 409 = 有进行中的作业
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}
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
        <h1 class="mk-page-title">机器列表</h1>
        <div class="mk-header-right">
          <span class="mk-subtle">{{ remainingSec }}s 后自动刷新</span>
          <el-button :icon="Refresh" circle aria-label="立即刷新" @click="refreshNow" />
        </div>
      </header>

      <el-card class="mk-card">
        <div class="mk-filters">
          <el-input
            v-model="filters.search"
            placeholder="搜索 序列号 / UUID / 型号 / BMC IP"
            clearable
            style="width: 300px"
          />
          <el-radio-group v-model="filters.status">
            <el-radio-button value="">全部状态</el-radio-button>
            <el-radio-button value="online">在线</el-radio-button>
            <el-radio-button value="offline">离线</el-radio-button>
          </el-radio-group>
          <el-radio-group v-model="filters.managed">
            <el-radio-button value="">全部纳管</el-radio-button>
            <el-radio-button value="yes">已纳管</el-radio-button>
            <el-radio-button value="no">未纳管</el-radio-button>
          </el-radio-group>
        </div>

        <el-table
          v-loading="loading"
          :data="filtered"
          stripe
          row-key="uuid"
          @row-click="openDetail"
          style="cursor: pointer"
        >
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <StatusTag :status="row.status" raw />
            </template>
          </el-table-column>
          <el-table-column label="IP 地址" min-width="210">
            <template #default="{ row }">
              <div v-if="row.ipv4_addresses?.length || row.bmc_ip" style="line-height: 1.5">
                <span v-if="row.ipv4_addresses?.length" class="mono">{{ row.ipv4_addresses.join('， ') }}</span>
                <span v-else class="mk-subtle">业务 —</span>
                <div v-if="row.bmc_ip">
                  <span class="mk-subtle" style="font-size: 12px">BMC </span>
                  <span class="mono">{{ row.bmc_ip }}</span>
                </div>
              </div>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="序列号" min-width="140">
            <template #default="{ row }">{{ row.serial || '—' }}</template>
          </el-table-column>
          <el-table-column label="UUID" min-width="240">
            <template #default="{ row }">
              <CopyableText :text="row.uuid" :label="row.uuid.slice(0, 13) + '…'" />
            </template>
          </el-table-column>
          <el-table-column label="机型" min-width="180">
            <template #default="{ row }">
              {{ [row.manufacturer, row.product_name].filter(Boolean).join(' ') || '—' }}
            </template>
          </el-table-column>
          <el-table-column label="BMC" width="70">
            <template #default="{ row }">
              <el-tag v-if="row.bmc_managed" type="success" size="small" disable-transitions>纳管</el-tag>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="最近上报" width="120">
            <template #default="{ row }">{{ fmtRelative(row.last_seen) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="130" fixed="right">
            <template #default="{ row }">
              <el-button
                text
                size="small"
                type="primary"
                :loading="installLoading"
                @click.stop="openInstall(asRow<MachineSummary>(row))"
              >装机</el-button>
              <el-button text size="small" type="danger" @click.stop="remove(asRow<MachineSummary>(row))">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="没有机器；让机器 PXE 引导进入 live 系统即可自动上报" :image-size="72" />
          </template>
        </el-table>
      </el-card>

      <!-- 列表页直达装机弹窗：数据按需拉取，保存后刷新列表 -->
      <InstallDialog
        v-model="installVisible"
        :machine-uuid="installUuid"
        :report="installReport"
        :images="installImages"
        :profiles="installProfiles"
        :subnets="installSubnets"
        :binding="installBinding"
        intent="install"
        @saved="load"
      />
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

.mk-header-right {
  display: flex;
  align-items: center;
  gap: 10px;
}
</style>
