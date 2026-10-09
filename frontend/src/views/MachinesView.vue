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
import { usePagedTable } from '@/composables/usePagedTable'
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
const installIntent = ref<'install' | 'reinstall'>('install')
const installReport = ref<Report | null>(null)
const installImages = ref<Image[]>([])
const installProfiles = ref<Profile[]>([])
const installSubnets = ref<Subnet[]>([])
const installBinding = ref<Binding | null>(null)
const installLoading = ref(false)

async function openInstall(row: MachineSummary, intent: 'install' | 'reinstall' = 'install'): Promise<void> {
  installLoading.value = true
  installUuid.value = row.uuid
  installIntent.value = intent
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

function openReinstall(row: MachineSummary): void {
  void openInstall(row, 'reinstall')
}

// ---------- 备注行内编辑（点击备注文字进入编辑，回车/失焦保存） ----------
const editingNotes = reactive({ uuid: null as string | null, value: '' })

function startEditNotes(row: MachineSummary): void {
  editingNotes.uuid = row.uuid
  editingNotes.value = row.notes ?? ''
}

// ---------- 位置行内编辑（与备注同一模式，独立存放互不干扰） ----------
const editingLoc = reactive({ uuid: null as string | null, value: '' })

function startEditLoc(row: MachineSummary): void {
  editingLoc.uuid = row.uuid
  editingLoc.value = row.location ?? ''
}

let locSaving = false
async function saveLoc(row: MachineSummary): Promise<void> {
  if (locSaving) return
  if (editingLoc.uuid !== row.uuid) return
  const next = editingLoc.value.trim()
  locSaving = true
  try {
    if (next !== (row.location ?? '')) {
      await machinesApi.setLocation(row.uuid, next)
      row.location = next
    }
  } catch (err) {
    ElMessage.error(`位置保存失败: ${(err as Error).message}`)
  } finally {
    locSaving = false
    editingLoc.uuid = null
  }
}

let notesSaving = false
async function saveNotes(row: MachineSummary): Promise<void> {
  if (notesSaving) return
  if (editingNotes.uuid !== row.uuid) return
  const next = editingNotes.value.trim()
  notesSaving = true
  try {
    if (next !== (row.notes ?? '')) {
      await machinesApi.setNotes(row.uuid, next)
      row.notes = next
    }
  } catch (err) {
    ElMessage.error(`备注保存失败: ${(err as Error).message}`)
  } finally {
    notesSaving = false
    editingNotes.uuid = null
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
        <h1 class="mk-page-title">机器</h1>
        <p class="mk-page-sub">裸机清单与装机入口 — 序列号 / UUID / BMC 一览</p>
        <div class="mk-header-actions">
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

        <el-table class="mk-table"
          v-loading="loading"
          :data="pagedTable.paged.value"
          stripe
          row-key="uuid"
          @row-click="openDetail"
          style="cursor: pointer"
        >
          <el-table-column label="状态" min-width="70">
            <template #default="{ row }">
              <StatusTag :status="row.status" raw />
            </template>
          </el-table-column>
          <el-table-column label="业务地址" min-width="170">
            <template #default="{ row }">
              <span v-if="row.install_ip" class="mono">{{ row.install_ip }}</span>
              <span v-else-if="row.ipv4_addresses?.length" class="mono">{{ row.ipv4_addresses.join('， ') }}</span>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="BMC 地址" min-width="110">
            <template #default="{ row }">
              <span v-if="row.bmc_ip" class="mono">{{ row.bmc_ip }}</span>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="序列号" min-width="90">
            <template #default="{ row }">{{ row.serial || '—' }}</template>
          </el-table-column>
          <el-table-column label="UUID" min-width="170">
            <template #default="{ row }">
              <CopyableText :text="row.uuid" :label="row.uuid" />
            </template>
          </el-table-column>
          <el-table-column label="机型" min-width="120">
            <template #default="{ row }">
              {{ [row.manufacturer, row.product_name].filter(Boolean).join(' ') || '—' }}
            </template>
          </el-table-column>
          <el-table-column label="BMC" min-width="56">
            <template #default="{ row }">
              <el-tag v-if="row.bmc_managed" type="success" size="small" disable-transitions>纳管</el-tag>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="位置" min-width="110">
            <template #default="{ row }">
              <el-tooltip v-if="row.location && row.location !== editingLoc.uuid" :content="row.location" placement="top">
                <span class="mk-notes" @click.stop="startEditLoc(asRow<MachineSummary>(row))">{{ row.location }}</span>
              </el-tooltip>
              <el-input
                v-if="editingLoc.uuid === row.uuid"
                v-model="editingLoc.value"
                size="small"
                maxlength="80"
                placeholder="机房/机柜/U位"
                @click.stop
                @keyup.enter="saveLoc(asRow<MachineSummary>(row))"
                @keyup.esc="editingLoc.uuid = null"
                @blur="saveLoc(asRow<MachineSummary>(row))"
              />
              <span
                v-else-if="!row.location"
                class="mk-subtle mk-notes-add"
                @click.stop="startEditLoc(asRow<MachineSummary>(row))"
              >＋ 位置</span>
            </template>
          </el-table-column>
          <el-table-column label="备注" min-width="150">
            <template #default="{ row }">
              <el-tooltip v-if="row.notes && row.notes !== editingNotes.uuid" :content="row.notes" placement="top">
                <span class="mk-notes" @click.stop="startEditNotes(asRow<MachineSummary>(row))">{{ row.notes }}</span>
              </el-tooltip>
              <el-input
                v-if="editingNotes.uuid === row.uuid"
                v-model="editingNotes.value"
                size="small"
                maxlength="200"
                placeholder="备注（如：库位 A-12 / 数据库节点勿动）"
                @click.stop
                @keyup.enter="saveNotes(asRow<MachineSummary>(row))"
                @keyup.esc="editingNotes.uuid = null"
                @blur="saveNotes(asRow<MachineSummary>(row))"
              />
              <span
                v-else-if="!row.notes"
                class="mk-subtle mk-notes-add"
                @click.stop="startEditNotes(asRow<MachineSummary>(row))"
              >＋ 备注</span>
            </template>
          </el-table-column>
          <el-table-column label="最近上报" min-width="90">
            <template #default="{ row }">{{ fmtRelative(row.last_seen) }}</template>
          </el-table-column>
          <el-table-column label="操作" min-width="150">
            <template #default="{ row }">
              <el-button text size="small" @click.stop="openDetail(asRow<MachineSummary>(row))">详情</el-button>
              <el-button
                text
                size="small"
                type="primary"
                :loading="installLoading"
                @click.stop="openInstall(asRow<MachineSummary>(row))"
              >装机</el-button>
              <el-button
                text
                size="small"
                type="danger"
                @click.stop="openReinstall(asRow<MachineSummary>(row))"
              >重装</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="没有机器；让机器 PXE 引导进入 live 系统即可自动上报" :image-size="72" />
          </template>
        </el-table>
        <el-pagination
          v-if="pagedTable.total.value > pagedTable.pageSize.value"
          v-model:current-page="pagedTable.currentPage.value"
          v-model:page-size="pagedTable.pageSize.value"
          :page-sizes="[20, 50, 100]"
          :total="pagedTable.total.value"
          layout="total, sizes, prev, pager, next, jumper"
          style="margin-top: 16px; justify-content: flex-end"
        />
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
        :intent="installIntent"
        @saved="load"
      />
    </div>
  </AppShell>
</template>

<style scoped>
/* 备注列：悬停显示可编辑暗示，与行点击下钻互不干扰（click.stop） */
.mk-notes,
.mk-notes-add {
  cursor: text;
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: bottom;
}

.mk-notes-add:hover,
.mk-notes:hover {
  color: var(--el-color-primary);
}

.mk-filters {
  display: flex;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}

.mk-header-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}
</style>
