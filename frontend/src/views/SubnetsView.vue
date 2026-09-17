<script setup lang="ts">
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'

import { subnetsApi } from '@/api'
import type { Subnet } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { fmtAbsolute } from '@/lib/format'
import { isValidCIDR, isValidIPv4, subnetRange } from '@/lib/net'
import { asRow } from '@/lib/typed'

interface PoolSegForm {
  start: string
  end: string
}

interface SubnetForm {
  id?: string
  name: string
  cidr: string
  gateway: string
  dns: string
  vlanId: number | undefined
  pools: PoolSegForm[]
}

const list = ref<Subnet[]>([])
const loading = ref(false)
const dialogVisible = ref(false)
const saving = ref(false)
const formRef = ref<FormInstance>()
const form = reactive<SubnetForm>({ name: '', cidr: '', gateway: '', dns: '', vlanId: undefined, pools: [{ start: '', end: '' }] })

const rules: FormRules = {
  name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
  cidr: [
    { required: true, message: '请输入 CIDR', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) => cb(isValidCIDR(v) ? undefined : new Error('CIDR 格式无效')),
      trigger: 'blur',
    },
  ],
  gateway: [
    {
      validator: (_r, v: string, cb) =>
        !v || isValidIPv4(v) ? cb() : cb(new Error('网关不是合法 IPv4')),
      trigger: 'blur',
    },
    {
      validator: (_r, v: string, callback) => {
        if (!v || !form.cidr) return callback()
        if (!isValidCIDR(form.cidr)) return callback()
        if (!subnetRange(form.cidr) || !isValidIPv4(v)) return callback()
        const range = subnetRange(form.cidr)!
        callback(
          v === range.network || v === range.broadcast
            ? new Error(`网关不能是网络地址（${range.network}）或广播地址（${range.broadcast}）`)
            : undefined,
        )
      },
      trigger: 'blur',
    },
  ],
}

onMounted(load)

function addPool(): void {
  form.pools.push({ start: '', end: '' })
}

function removePool(i: number): void {
  form.pools.splice(i, 1)
}

// validatePools checks all segments together; called from save() before
// submit (array fields don't map cleanly onto per-field el-form rules).
function validatePools(): string | undefined {
  const segs = form.pools.filter((p) => p.start.trim() || p.end.trim())
  const range = isValidCIDR(form.cidr) ? subnetRange(form.cidr) : null
  const seen: Array<[string, string]> = []
  for (let i = 0; i < segs.length; i++) {
    const a = segs[i].start.trim(), b = segs[i].end.trim()
    if (!a || !b) return '地址池每段的起始和结束需同时填写，或同时清空'
    if (!isValidIPv4(a)) return `池段 ${i + 1} 起始不是合法 IPv4`
    if (!isValidIPv4(b)) return `池段 ${i + 1} 结束不是合法 IPv4`
    if (range) {
      if (a === range.network || a === range.broadcast) return `池段 ${i + 1} 起始不能是网络/广播地址`
      if (b === range.network || b === range.broadcast) return `池段 ${i + 1} 结束不能是网络/广播地址`
    }
    const na = ipToNumber(a), nb = ipToNumber(b)
    if (na !== null && nb !== null && na > nb) return `池段 ${i + 1} 起始大于结束`
    for (const [ps, pe] of seen) {
      const pse = ipToNumber(ps), pee = ipToNumber(pe)
      if (na !== null && nb !== null && pse !== null && pee !== null && na <= pee && pse <= nb)
        return `池段 ${i + 1} 与已有池段重叠`
    }
    seen.push([a, b])
  }
  return undefined
}

function formatPools(ranges: Array<{ start: string; end: string }>): string {
  return ranges.map((r) => `${r.start}~${r.end}`).join('， ')
}

function ipToNumber(ip: string): number | null {
  const parts = ip.split('.')
  if (parts.length !== 4) return null
  let n = 0
  for (const p of parts) {
    const v = Number(p)
    if (!Number.isInteger(v) || v < 0 || v > 255) return null
    n = n * 256 + v
  }
  return n
}

async function load(): Promise<void> {
  loading.value = true
  try {
    list.value = await subnetsApi.list()
  } catch (err) {
    ElMessage.error(`加载子网失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  Object.assign(form, { id: undefined, name: '', cidr: '', gateway: '', dns: '', vlanId: undefined, pools: [{ start: '', end: '' }] })
  dialogVisible.value = true
}

function openEdit(row: Subnet): void {
  Object.assign(form, {
    id: row.id,
    name: row.name,
    cidr: row.cidr,
    gateway: row.gateway ?? '',
    dns: (row.dns ?? []).join(', '),
    vlanId: row.vlan_id,
    pools: (row.dhcp_ranges ?? []).map((r) => ({ start: r.start, end: r.end })),
  })
  if (form.pools.length === 0) form.pools.push({ start: '', end: '' })
  dialogVisible.value = true
}

async function save(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
  const poolErr = validatePools()
  if (poolErr) {
    ElMessage.warning(poolErr)
    return
  }
  saving.value = true
  try {
    const payload = {
      name: form.name.trim(),
      cidr: form.cidr.trim(),
      gateway: form.gateway.trim() || undefined,
      dns: form.dns
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean),
      vlan_id: form.vlanId,
      dhcp_ranges: form.pools
        .filter((p) => p.start.trim() && p.end.trim())
        .map((p) => ({ start: p.start.trim(), end: p.end.trim() })),
    }
    if (form.id) {
      await subnetsApi.update(form.id, payload)
      ElMessage.success('子网已更新')
    } else {
      await subnetsApi.create(payload)
      ElMessage.success('子网已创建')
    }
    dialogVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}

async function remove(row: Subnet): Promise<void> {
  try {
    await ElMessageBox.confirm(`确定删除子网「${row.name}」？`, '删除子网', { type: 'warning' })
  } catch {
    return
  }
  try {
    await subnetsApi.remove(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    // 422 = 被 profile 引用
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">子网</h1>
        <el-button type="primary" @click="openCreate">新建子网</el-button>
      </header>

      <el-card class="mk-card">
        <el-table v-loading="loading" :data="list" stripe>
          <el-table-column prop="name" label="名称" min-width="140" />
          <el-table-column prop="cidr" label="CIDR" min-width="160">
            <template #default="{ row }"><span class="mono">{{ row.cidr }}</span></template>
          </el-table-column>
          <el-table-column prop="gateway" label="网关" min-width="130">
            <template #default="{ row }"><span class="mono">{{ row.gateway || '—' }}</span></template>
          </el-table-column>
          <el-table-column label="DNS" min-width="160">
            <template #default="{ row }">
              <span class="mono">{{ (row.dns ?? []).join(', ') || '—' }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="vlan_id" label="VLAN" width="80">
            <template #default="{ row }">{{ row.vlan_id ?? '—' }}</template>
          </el-table-column>
          <el-table-column label="DHCP 池" min-width="230">
            <template #default="{ row }">
              <span v-if="row.dhcp_ranges?.length" class="mono">
                {{ formatPools(row.dhcp_ranges) }}
              </span>
              <span v-else>—</span>
            </template>
          </el-table-column>
          <el-table-column label="创建时间" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="140" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="openEdit(asRow<Subnet>(row))">编辑</el-button>
              <el-button text size="small" type="danger" @click="remove(asRow<Subnet>(row))">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="还没有子网" :image-size="72" />
          </template>
        </el-table>
      </el-card>

      <el-dialog
        v-model="dialogVisible"
        :title="form.id ? '编辑子网' : '新建子网'"
        width="480px"
        destroy-on-close
      >
        <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
          <el-form-item label="名称" prop="name">
            <el-input v-model="form.name" placeholder="如 mgmt-legacy" />
          </el-form-item>
          <el-form-item label="CIDR" prop="cidr">
            <el-input v-model="form.cidr" placeholder="192.168.10.0/24" class="mono" />
          </el-form-item>
          <el-form-item label="网关" prop="gateway">
            <el-input v-model="form.gateway" placeholder="192.168.10.1" class="mono" />
          </el-form-item>
          <el-form-item label="DNS">
            <el-input v-model="form.dns" placeholder="多个用逗号分隔" class="mono" />
          </el-form-item>
          <el-form-item label="VLAN">
            <el-input-number v-model="form.vlanId" :min="1" :max="4094" placeholder="可选" />
          </el-form-item>
          <el-form-item
            v-for="(seg, i) in form.pools"
            :key="i"
            :label="i === 0 ? '地址池' : ''"
            :prop="'pools.' + i"
          >
            <div style="display: flex; gap: 8px; width: 100%">
              <el-input v-model="seg.start" placeholder="起始，如 192.168.1.100" class="mono" style="flex: 1" />
              <span style="align-self: center">~</span>
              <el-input v-model="seg.end" placeholder="结束，如 192.168.1.150" class="mono" style="flex: 1" />
              <el-button
                v-if="form.pools.length > 1 || seg.start || seg.end"
                text
                type="danger"
                @click="removePool(i)"
              >删除</el-button>
            </div>
          </el-form-item>
          <el-form-item>
            <el-button text type="primary" @click="addPool">+ 添加地址段</el-button>
          </el-form-item>
          <div style="margin: 0 0 12px 90px; font-size: 12px; color: var(--el-text-color-secondary); line-height: 1.5">
            填写 DHCP 池后，从其他网段经 DHCP 中继（giaddr）转发来的请求将分配该范围内的地址；
            对端网关/SVI 地址需与本子网同网段。留空表示该子网仅用于静态分配。
          </div>
        </el-form>
        <template #footer>
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </template>
      </el-dialog>
    </div>
  </AppShell>
</template>
