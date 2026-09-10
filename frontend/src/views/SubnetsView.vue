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

interface SubnetForm {
  id?: string
  name: string
  cidr: string
  gateway: string
  dns: string
  vlanId: number | undefined
}

const list = ref<Subnet[]>([])
const loading = ref(false)
const dialogVisible = ref(false)
const saving = ref(false)
const formRef = ref<FormInstance>()
const form = reactive<SubnetForm>({ name: '', cidr: '', gateway: '', dns: '', vlanId: undefined })

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
  Object.assign(form, { id: undefined, name: '', cidr: '', gateway: '', dns: '', vlanId: undefined })
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
  })
  dialogVisible.value = true
}

async function save(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
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
        </el-form>
        <template #footer>
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </template>
      </el-dialog>
    </div>
  </AppShell>
</template>
