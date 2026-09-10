<script setup lang="ts">
import { Refresh } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'

import { bmcApi, bindingsApi, imagesApi, jobsApi, machinesApi, profilesApi, subnetsApi } from '@/api'
import type { Binding, Image, Job, Profile, Report, ReportMeta, Subnet } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import CopyableText from '@/components/CopyableText.vue'
import InstallDialog from '@/components/InstallDialog.vue'
import { usePollHealth } from '@/composables/usePollHealth'
import StatusTag from '@/components/StatusTag.vue'
import { fmtAbsolute, fmtBytes, fmtRelative } from '@/lib/format'
import { asRow } from '@/lib/typed'

const route = useRoute()
const uuid = computed(() => String(route.params.uuid ?? ''))

const loading = ref(false)
const report = ref<Report | null>(null)
const summary = ref<{ status?: string; bmc_ip?: string; bmc_managed?: boolean; serial?: string; last_seen?: string } | null>(null)

// 相关资源
const binding = ref<Binding | null>(null)
const images = ref<Image[]>([])
const profiles = ref<Profile[]>([])
const subnets = ref<Subnet[]>([])
const recentJobs = ref<Job[]>([])
const reportHistory = ref<ReportMeta[]>([])

// 弹窗
const installVisible = ref(false)
const unbindConfirmVisible = ref(false)
const unbindBusy = ref(false)

// 原始 JSON 展示
const rawJsonVisible = ref(false)

const health = usePollHealth()

async function load(): Promise<void> {
  if (!uuid.value) return
  loading.value = true
  try {
    const [machines, rep, bindings] = await Promise.all([
      machinesApi.list(),
      machinesApi.get(uuid.value).catch(() => null),
      bindingsApi.get(uuid.value).catch(() => null),
    ])
    summary.value = machines.find((m) => m.uuid === uuid.value) ?? null
    report.value = rep
    binding.value = bindings
    const [imgs, profs, subs, jobs, history] = await Promise.all([
      imagesApi.list(),
      profilesApi.list(),
      subnetsApi.list(),
      jobsApi.list({ machine_uuid: uuid.value, limit: 10 }),
      machinesApi.reports(uuid.value).catch(() => []),
    ])
    images.value = imgs
    profiles.value = profs
    subnets.value = subs
    recentJobs.value = jobs
    reportHistory.value = history.slice(0, 20)
    health.noteOk()
  } catch {
    health.noteError()
  } finally {
    loading.value = false
  }
}

// 轮询：详情页 10s（装机期间作业状态变化主要看作业页）
let pollTimer: ReturnType<typeof setInterval> | null = null
onMounted(() => {
  void load()
  pollTimer = setInterval(() => {
    if (!document.hidden) void load()
  }, 10_000)
})
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

const bindingImage = computed(() =>
  binding.value?.image_id ? images.value.find((i) => i.id === binding.value?.image_id) : undefined,
)
const bindingProfile = computed(() =>
  binding.value?.profile_id
    ? profiles.value.find((p) => p.id === binding.value?.profile_id)
    : undefined,
)
const bindingSubnet = computed(() =>
  binding.value?.subnet_id ? subnets.value.find((s) => s.id === binding.value?.subnet_id) : undefined,
)

const biosVersion = computed(() => {
  const bios = (report.value?.firmware as { bios?: { vendor?: string; version?: string } } | undefined)?.bios
  return bios ? [bios.vendor, bios.version].filter(Boolean).join(' ') : ''
})

const isInstalling = computed(() =>
  recentJobs.value.some((j) => j.status === 'pending' || j.status === 'running'),
)

async function unbind(): Promise<void> {
  if (!binding.value) return
  unbindBusy.value = true
  try {
    await bindingsApi.remove(uuid.value)
    ElMessage.success('已删除绑定')
    unbindConfirmVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(`删除绑定失败: ${(err as Error).message}`)
  } finally {
    unbindBusy.value = false
  }
}

async function syncBmc(): Promise<void> {
  if (!summary.value?.bmc_ip) return
  const cred = await bmcApi.list().then((l) => l.find((c) => c.ip === summary.value?.bmc_ip))
  if (!cred) {
    ElMessage.warning(`未找到 ${summary.value.bmc_ip} 的 BMC 凭据，请先在 BMC 页添加`)
    return
  }
  try {
    await bmcApi.onboard(cred.machine_uuid)
    ElMessage.success('已下发 PXE 引导，机器将重启进入 live 系统重新上报')
  } catch (err) {
    ElMessage.error(`操作失败: ${(err as Error).message}`)
  }
}

function viewHistoryItem(meta: ReportMeta): void {
  void machinesApi.report(uuid.value, meta.id).then((rep) => {
    report.value = rep
    rawJsonVisible.value = false
  })
}
</script>

<template>
  <AppShell>
    <div v-loading="loading" class="mk-page">
      <el-alert
        v-if="health.unreachable.value"
        title="无法连接 controller — 数据可能已过期，正在持续重试"
        type="error"
        show-icon
        :closable="false"
        style="margin-bottom: 12px"
      />
      <header class="mk-page-header">
        <h1 class="mk-page-title">机器详情</h1>
        <div class="mk-header-right">
          <el-button v-if="summary?.bmc_ip" @click="syncBmc">同步 BMC</el-button>
          <el-button :icon="Refresh" circle aria-label="刷新" @click="load" />
        </div>
      </header>

      <!-- 头部摘要卡 -->
      <el-card class="mk-card">
        <div class="mk-summary">
          <div class="mk-summary-main">
            <StatusTag :status="summary?.status" raw />
            <span class="mk-machine-name">
              {{ summary?.serial || report?.machine?.serial || '未知序列号' }}
            </span>
            <span class="mk-muted">
              {{ [report?.machine?.manufacturer, report?.machine?.product_name].filter(Boolean).join(' ') }}
            </span>
          </div>
          <dl class="mk-kv-grid" style="margin: 10px 0 0">
            <div class="mk-kv"><dt>UUID</dt><dd><CopyableText :text="uuid" /></dd></div>
            <div class="mk-kv"><dt>最近上报</dt><dd>{{ fmtRelative(summary?.last_seen) }}</dd></div>
            <div class="mk-kv"><dt>采集时间</dt><dd>{{ fmtAbsolute(report?.collected_at) }}</dd></div>
            <div class="mk-kv">
              <dt>BMC</dt>
              <dd>
                <template v-if="summary?.bmc_ip">
                  {{ summary.bmc_ip }}
                  <el-tag v-if="summary.bmc_managed" size="small" type="success">纳管</el-tag>
                </template>
                <span v-else class="mk-subtle">未上报</span>
              </dd>
            </div>
            <div class="mk-kv"><dt>CPU 架构</dt><dd>{{ report?.cpu?.arch || '—' }}</dd></div>
            <div class="mk-kv"><dt>固件</dt><dd>{{ biosVersion || '—' }}</dd></div>
            <div class="mk-kv"><dt>Live 版本</dt><dd>{{ (report?.system as any)?.live_image_version || '—' }}</dd></div>
          </dl>
        </div>
      </el-card>

      <!-- 装机管理面板 -->
      <el-card class="mk-card">
        <template #header>
          <div class="mk-card-header">
            <span>装机管理</span>
            <div>
              <template v-if="binding">
                <el-button type="primary" @click="installVisible = true">
                  {{ isInstalling ? '查看装机参数' : '修改装机配置' }}
                </el-button>
                <el-button type="danger" plain @click="unbindConfirmVisible = true">删除绑定</el-button>
              </template>
              <el-button v-else type="primary" @click="installVisible = true">配置装机</el-button>
            </div>
          </div>
        </template>

        <template v-if="binding">
          <dl class="mk-kv-grid">
            <div class="mk-kv"><dt>期望状态</dt><dd>{{ binding.desired_state ?? 'none' }}</dd></div>
            <div class="mk-kv"><dt>镜像</dt><dd>{{ bindingImage ? `${bindingImage.name} ${bindingImage.version}` : binding.image_id || '—' }}</dd></div>
            <div class="mk-kv"><dt>Profile</dt><dd>{{ bindingProfile?.name ?? binding.profile_id ?? '—' }}</dd></div>
            <div class="mk-kv"><dt>子网</dt><dd>{{ bindingSubnet?.name ?? binding.subnet_id ?? 'DHCP' }}</dd></div>
            <div class="mk-kv"><dt>静态 IP</dt><dd>{{ binding.static_address || '自动分配' }}</dd></div>
          </dl>
        </template>
        <el-empty
          v-else
          description="尚未配置：选择镜像与安装参数后，机器将自动进入装机流程"
          :image-size="60"
        />
      </el-card>

      <!-- 最近作业 -->
      <el-card class="mk-card">
        <template #header>
          <div class="mk-card-header">
            <span>最近作业</span>
            <router-link to="/jobs"><el-button text size="small">全部作业 →</el-button></router-link>
          </div>
        </template>
        <el-table :data="recentJobs" stripe size="small">
          <el-table-column label="状态" width="90">
            <template #default="{ row }"><StatusTag :status="row.status" /></template>
          </el-table-column>
          <el-table-column prop="type" label="类型" width="90" />
          <el-table-column label="创建时间" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.created_at) }}</template>
          </el-table-column>
          <el-table-column prop="stage" label="阶段" min-width="140" />
          <el-table-column prop="error" label="错误" min-width="180">
            <template #default="{ row }">
              <span v-if="row.error" class="mk-error">{{ row.error }}</span>
              <span v-else class="mk-subtle">—</span>
            </template>
          </el-table-column>
          <el-table-column label="" width="70">
            <template #default="{ row }">
              <router-link :to="`/jobs/${row.id}`"><el-button text size="small">详情</el-button></router-link>
            </template>
          </el-table-column>
          <template #empty>
            <span class="mk-subtle">还没有作业</span>
          </template>
        </el-table>
      </el-card>

      <!-- 上报历史 -->
      <el-card class="mk-card">
        <template #header><span>上报历史（最近 20 次）</span></template>
        <el-table :data="reportHistory" stripe size="small">
          <el-table-column prop="id" label="ID" width="80" />
          <el-table-column label="采集时间" width="180">
            <template #default="{ row }">{{ fmtAbsolute(row.collected_at) }}</template>
          </el-table-column>
          <el-table-column prop="agent_version" label="Agent 版本" min-width="120" />
          <el-table-column label="" width="80">
            <template #default="{ row }">
              <el-button text size="small" @click="viewHistoryItem(asRow<ReportMeta>(row))">查看</el-button>
            </template>
          </el-table-column>
          <template #empty><span class="mk-subtle">暂无历史</span></template>
        </el-table>
      </el-card>

      <!-- 硬件报告折叠区 -->
      <el-card class="mk-card">
        <template #header><span>硬件报告</span></template>
        <el-collapse>
          <el-collapse-item title="机器 / 固件" name="machine">
            <dl class="mk-kv-grid">
              <div class="mk-kv"><dt>厂商</dt><dd>{{ (report?.machine as any)?.manufacturer || '—' }}</dd></div>
              <div class="mk-kv"><dt>产品名</dt><dd>{{ (report?.machine as any)?.product_name || '—' }}</dd></div>
              <div class="mk-kv"><dt>序列号</dt><dd>{{ (report?.machine as any)?.serial || '—' }}</dd></div>
              <div class="mk-kv"><dt>SMBIOS UUID</dt><dd><span class="mono">{{ (report?.machine as any)?.smbios_uuid || '—' }}</span></dd></div>
              <div class="mk-kv"><dt>BIOS</dt><dd>{{ biosVersion || '—' }}</dd></div>
              <div class="mk-kv"><dt>UEFI 模式</dt><dd>{{ (report?.firmware as any)?.uefi_mode ? '是' : '否' }}</dd></div>
              <div class="mk-kv"><dt>Secure Boot</dt><dd>{{ (report?.firmware as any)?.secure_boot ? '启用' : '未启用' }}</dd></div>
              <div class="mk-kv"><dt>TPM</dt><dd>{{ (report?.firmware as any)?.tpm || '—' }}</dd></div>
            </dl>
          </el-collapse-item>

          <el-collapse-item title="CPU" name="cpu">
            <dl class="mk-kv-grid">
              <div class="mk-kv"><dt>架构</dt><dd>
                <el-tag v-if="report?.cpu?.arch" :type="report?.cpu?.arch === 'arm64' ? 'warning' : 'info'" size="small" disable-transitions>
                  {{ report?.cpu?.arch }}
                </el-tag>
                <span v-else class="mk-subtle">未上报</span>
              </dd></div>
              <div class="mk-kv"><dt>插槽数</dt><dd>{{ report?.cpu?.sockets ?? '—' }}</dd></div>
              <div class="mk-kv"><dt>物理核</dt><dd>{{ report?.cpu?.total_cores ?? '—' }}</dd></div>
              <div class="mk-kv"><dt>逻辑核</dt><dd>{{ report?.cpu?.total_threads ?? '—' }}</dd></div>
            </dl>
          </el-collapse-item>

          <el-collapse-item :title="`内存（${fmtBytes(report?.memory?.total_bytes)}）`" name="memory">
            <el-table :data="report?.memory?.dimms ?? []" stripe size="small">
              <el-table-column prop="slot" label="插槽" width="100" />
              <el-table-column label="容量" width="110">
                <template #default="{ row }">{{ fmtBytes(row.size_bytes) }}</template>
              </el-table-column>
              <el-table-column prop="speed_mt_s" label="频率 (MT/s)" width="110" />
              <el-table-column prop="type" label="类型" width="100" />
              <el-table-column prop="manufacturer" label="厂商" min-width="120" />
              <template #empty><span class="mk-subtle">无 DIMM 明细</span></template>
            </el-table>
          </el-collapse-item>

          <el-collapse-item title="磁盘" name="disks">
            <el-table :data="report?.disks ?? []" stripe size="small">
              <el-table-column prop="kname" label="设备" width="80" />
              <el-table-column prop="type" label="类型" width="70" />
              <el-table-column label="容量" width="100">
                <template #default="{ row }">{{ fmtBytes(row.size_bytes) }}</template>
              </el-table-column>
              <el-table-column prop="model" label="型号" min-width="160">
                <template #default="{ row }">{{ row.model || '—' }}</template>
              </el-table-column>
              <el-table-column label="WWN" min-width="180">
                <template #default="{ row }"><span class="mono">{{ row.wwn || '—' }}</span></template>
              </el-table-column>
              <el-table-column label="路径" min-width="160">
                <template #default="{ row }"><span class="mono">{{ row.path }}</span></template>
              </el-table-column>
              <template #empty><span class="mk-subtle">未检出磁盘</span></template>
            </el-table>
          </el-collapse-item>

          <el-collapse-item title="网卡" name="nics">
            <el-table :data="report?.nics ?? []" stripe size="small">
              <el-table-column prop="name" label="接口" width="90" />
              <el-table-column label="MAC" min-width="170">
                <template #default="{ row }"><span class="mono">{{ row.mac }}</span></template>
              </el-table-column>
              <el-table-column label="速率" width="100">
                <template #default="{ row }">{{ row.speed_mbps ? `${row.speed_mbps} Mb/s` : '—' }}</template>
              </el-table-column>
              <el-table-column label="状态" width="70">
                <template #default="{ row }">
                  <el-tag :type="row.link ? 'success' : 'info'" size="small">{{ row.link ? 'UP' : 'DOWN' }}</el-tag>
                </template>
              </el-table-column>
              <el-table-column prop="driver" label="驱动" width="110" />
              <el-table-column label="地址" min-width="180">
                <template #default="{ row }">
                  <span class="mono">{{ (row.addresses ?? []).join(', ') || '—' }}</span>
                </template>
              </el-table-column>
              <template #empty><span class="mk-subtle">未检出网卡</span></template>
            </el-table>
          </el-collapse-item>

          <el-collapse-item title="PCI 设备 / 加速卡 / 传感器 / BMC" name="misc">
            <dl class="mk-kv-grid">
              <div class="mk-kv"><dt>PCI 设备数</dt><dd>{{ report?.pci_devices?.length ?? 0 }}</dd></div>
              <div class="mk-kv"><dt>加速卡数</dt><dd>{{ report?.accelerators?.length ?? 0 }}</dd></div>
              <div class="mk-kv"><dt>传感器数</dt><dd>{{ report?.sensors?.length ?? 0 }}</dd></div>
              <div class="mk-kv"><dt>BMC 固件</dt><dd>{{ (report?.bmc as any)?.firmware_version || '—' }}</dd></div>
              <div class="mk-kv"><dt>BMC IP</dt><dd>{{ (report?.bmc as any)?.ip || '—' }}</dd></div>
            </dl>
          </el-collapse-item>

          <el-collapse-item title="系统 / Agent" name="system">
            <dl class="mk-kv-grid">
              <div class="mk-kv"><dt>内核</dt><dd>{{ (report?.system as any)?.kernel_release || '—' }}</dd></div>
              <div class="mk-kv"><dt>主机名</dt><dd>{{ (report?.system as any)?.hostname || '—' }}</dd></div>
              <div class="mk-kv"><dt>运行时长</dt><dd>{{ (report?.system as any)?.uptime_seconds ?? '—' }} s</dd></div>
              <div class="mk-kv"><dt>Agent 版本</dt><dd>{{ report?.agent?.version || '—' }}</dd></div>
            </dl>
            <div v-if="report?.agent?.errors?.length" class="mk-agent-errors">
              <div v-for="(e, i) in report.agent.errors" :key="i" class="mk-subtle">采集中出错：{{ e }}</div>
            </div>
          </el-collapse-item>

          <el-collapse-item title="原始 JSON" name="raw">
            <el-button size="small" @click="rawJsonVisible = !rawJsonVisible">
              {{ rawJsonVisible ? '收起' : '展开' }}原始 JSON
            </el-button>
            <pre v-if="rawJsonVisible" class="mk-raw-json">{{ JSON.stringify(report, null, 2) }}</pre>
          </el-collapse-item>
        </el-collapse>
      </el-card>

      <!-- 装机弹窗 -->
      <InstallDialog
        v-model="installVisible"
        :machine-uuid="uuid"
        :report="report"
        :images="images"
        :profiles="profiles"
        :subnets="subnets"
        :binding="binding"
        @saved="load"
      />

      <!-- 删除绑定确认 -->
      <el-dialog v-model="unbindConfirmVisible" title="删除绑定" width="420px">
        <p>
          删除后机器将不再自动装机（进行中的作业不受影响）。确定删除
          <span class="mono">{{ uuid.slice(0, 13) }}…</span> 的绑定？
        </p>
        <template #footer>
          <el-button @click="unbindConfirmVisible = false">取消</el-button>
          <el-button type="danger" :loading="unbindBusy" @click="unbind">删除</el-button>
        </template>
      </el-dialog>
    </div>
  </AppShell>
</template>

<style scoped>
.mk-header-right {
  display: flex;
  gap: 10px;
  align-items: center;
}

.mk-summary-main {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.mk-machine-name {
  font-size: 16px;
  font-weight: 600;
}

.mk-card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.mk-error {
  color: var(--el-color-danger);
  font-size: 12px;
}

.mk-raw-json {
  background: var(--el-fill-color-light);
  border-radius: 8px;
  padding: 12px;
  font-family: var(--mk-mono);
  font-size: 12px;
  max-height: 420px;
  overflow: auto;
  margin-top: 10px;
}

.mk-agent-errors {
  margin-top: 8px;
}
</style>
