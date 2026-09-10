<script setup lang="ts">
import { ArrowDown, Plus } from '@element-plus/icons-vue'
import type { FormInstance, FormRules, UploadFile } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'

import { bmcApi } from '@/api'
import type { BmcCredential } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { parseCsv } from '@/lib/csv'
import { asRow } from '@/lib/typed'
import { fmtAbsolute } from '@/lib/format'
import { isValidIPv4 } from '@/lib/net'

const list = ref<BmcCredential[]>([])
const loading = ref(false)

// 凭据表单
const dialogVisible = ref(false)
const saving = ref(false)
const formRef = ref<FormInstance>()
const form = reactive({
  machineUuid: '',
  ip: '',
  name: '',
  username: '',
  password: '',
})
const rules: FormRules = {
  ip: [
    { required: true, message: '请输入 BMC IP', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) => cb(isValidIPv4(v) ? undefined : new Error('地址须为 IPv4')),
      trigger: 'blur',
    },
  ],
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

// 电源操作确认

const POWER_ACTIONS = [
  { action: 'on', label: '开机', danger: false },
  { action: 'soft', label: '软关机', danger: true },
  { action: 'off', label: '硬关机', danger: true },
  { action: 'cycle', label: '强制重启', danger: true },
  { action: 'reset', label: '硬复位', danger: true },
] as const

// CSV 导入
const importVisible = ref(false)
const importText = ref('')
const importing = ref(false)
const importFileRef = ref<UploadFile | null>(null)

onMounted(load)

async function load(): Promise<void> {
  loading.value = true
  try {
    list.value = await bmcApi.list()
  } catch (err) {
    ElMessage.error(`加载 BMC 凭据失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  Object.assign(form, { machineUuid: '', ip: '', name: '', username: '', password: '' })
  dialogVisible.value = true
}

function openEdit(row: BmcCredential): void {
  Object.assign(form, {
    machineUuid: row.machine_uuid,
    ip: row.ip,
    name: row.name ?? '',
    username: row.username,
    password: '',
  })
  dialogVisible.value = true
}

async function save(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
  saving.value = true
  try {
    if (form.machineUuid) {
      // 编辑（PUT /bmc/{uuid}）：密码留空表示不修改
      const payload: Record<string, unknown> = {
        ip: form.ip.trim(),
        username: form.username.trim(),
        ...(form.name ? { name: form.name.trim() } : {}),
      }
      if (form.password) payload.password = form.password
      await bmcApi.update(form.machineUuid, payload)
      ElMessage.success('凭据已更新')
    } else {
      await bmcApi.create({
        ip: form.ip.trim(),
        username: form.username.trim(),
        password: form.password,
        ...(form.name ? { name: form.name.trim() } : {}),
      })
      ElMessage.success('凭据已创建（未上报机器将使用占位 UUID，待 PXE 上报后自动合并）')
    }
    dialogVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}

async function remove(row: BmcCredential): Promise<void> {
  try {
    await ElMessageBox.confirm(`确定删除 ${row.ip} 的凭据？`, '删除凭据', { type: 'warning' })
  } catch {
    return
  }
  try {
    await bmcApi.remove(row.machine_uuid)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}

async function test(row: BmcCredential): Promise<void> {
  try {
    const res = await bmcApi.test(row.machine_uuid)
    if (res.ok) ElMessage.success(`${row.ip}: 连接正常，电源状态 ${res.power ?? '未知'}`)
    else ElMessage.error(`${row.ip}: ${res.error ?? '连接失败'}`)
  } catch (err) {
    ElMessage.error(`测试失败: ${(err as Error).message}`)
  }
}

async function onboard(row: BmcCredential): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `让 ${row.ip} PXE 重启进入 live 系统（只上报库存，不装机）？`,
      'PXE 纳管',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await bmcApi.onboard(row.machine_uuid)
    ElMessage.success(`已下发 PXE 引导指令，等待机器上报`)
  } catch (err) {
    ElMessage.error(`操作失败: ${(err as Error).message}`)
  }
}

async function power(row: BmcCredential, action: string, danger: boolean): Promise<void> {
  let powerAction = action
  if (danger) {
    try {
      await ElMessageBox.prompt(
        `危险操作。输入「确认」以对 ${row.ip} 执行「${POWER_ACTIONS.find((a) => a.action === action)?.label}」。`,
        '确认电源操作',
        {
          type: 'warning',
          inputPattern: /^确认$/,
          inputErrorMessage: '请输入「确认」',
        },
      )
    } catch {
      return
    }
  }
  try {
    await bmcApi.power(row.machine_uuid, powerAction as 'on' | 'off' | 'cycle' | 'soft' | 'reset')
    ElMessage.success(`${row.ip}: ${powerAction} 已执行`)
  } catch (err) {
    ElMessage.error(`操作失败: ${(err as Error).message}`)
  }
}

function onImportFileChange(f: UploadFile): void {
  importFileRef.value = f
  if (f.raw) {
    const reader = new FileReader()
    reader.onload = () => {
      importText.value = String(reader.result ?? '')
    }
    reader.readAsText(f.raw)
  }
}

async function doImport(): Promise<void> {
  const rows = parseCsv(importText.value)
    .map((cells) => cells.map((c) => c.trim()))
    .filter((cells) => cells.length >= 3 && cells[0] !== '')

  let ok = 0
  const errors: string[] = []
  importing.value = true
  try {
    for (const cells of rows) {
      const [ip, username, password] = cells
      if (!isValidIPv4(ip)) {
        errors.push(`${ip}: 非 IPv4`)
        continue
      }
      try {
        await bmcApi.create({ ip, username, password })
        ok++
      } catch (err) {
        errors.push(`${ip}: ${(err as Error).message}`)
      }
    }
    ElMessage.success(`导入完成：成功 ${ok}，失败 ${errors.length}`)
    if (errors.length) console.warn('BMC 导入失败明细:', errors)
    importVisible.value = false
    importText.value = ''
    importFileRef.value = null
    await load()
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">BMC 凭据</h1>
        <div>
          <el-button @click="importVisible = true">CSV 导入</el-button>
          <el-button type="primary" :icon="Plus" @click="openCreate">新建凭据</el-button>
        </div>
      </header>

      <el-card class="mk-card">
        <el-table v-loading="loading" :data="list" stripe>
          <el-table-column prop="ip" label="BMC IP" min-width="140">
            <template #default="{ row }"><span class="mono">{{ row.ip }}</span></template>
          </el-table-column>
          <el-table-column prop="name" label="名称" min-width="120">
            <template #default="{ row }">{{ row.name || '—' }}</template>
          </el-table-column>
          <el-table-column prop="machine_uuid" label="机器 UUID" min-width="200">
            <template #default="{ row }"><span class="mono mk-subtle">{{ row.machine_uuid }}</span></template>
          </el-table-column>
          <el-table-column prop="username" label="用户名" min-width="110" />
          <el-table-column label="更新时间" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.updated_at ?? row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="300" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="test(asRow<BmcCredential>(row))">测试</el-button>
              <el-button text size="small" @click="onboard(asRow<BmcCredential>(row))">PXE 纳管</el-button>
              <el-dropdown trigger="click" @command="(cmd: string) => power(asRow<BmcCredential>(row), cmd, cmd !== 'on')">
                <el-button text size="small" type="primary">
                  电源<el-icon class="el-icon--right"><ArrowDown /></el-icon>
                </el-button>
                <template #dropdown>
                  <el-dropdown-menu>
                    <el-dropdown-item
                      v-for="a in POWER_ACTIONS"
                      :key="a.action"
                      :command="a.action"
                      :divided="a.danger"
                    >
                      {{ a.label }}
                    </el-dropdown-item>
                  </el-dropdown-menu>
                </template>
              </el-dropdown>
              <el-button text size="small" @click="openEdit(asRow<BmcCredential>(row))">编辑</el-button>
              <el-button text size="small" type="danger" @click="remove(asRow<BmcCredential>(row))">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="还没有 BMC 凭据" :image-size="72" />
          </template>
        </el-table>
      </el-card>

      <el-dialog
        v-model="dialogVisible"
        :title="form.machineUuid ? '编辑凭据' : '新建凭据'"
        width="440px"
        destroy-on-close
      >
        <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
          <el-form-item label="BMC IP" prop="ip">
            <el-input v-model="form.ip" placeholder="192.168.10.100" class="mono" :disabled="!!form.machineUuid" />
          </el-form-item>
          <el-form-item label="名称">
            <el-input v-model="form.name" placeholder="可选，如 机房A-节点3" />
          </el-form-item>
          <el-form-item label="用户名" prop="username">
            <el-input v-model="form.username" placeholder="如 ADMIN" />
          </el-form-item>
          <el-form-item label="密码" prop="password" :rules="form.machineUuid ? [] : rules.password">
            <el-input
              v-model="form.password"
              type="password"
              show-password
              :placeholder="form.machineUuid ? '留空表示不修改' : 'BMC 密码'"
            />
          </el-form-item>
        </el-form>
        <template #footer>
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </template>
      </el-dialog>

      <el-dialog v-model="importVisible" title="CSV 批量导入" width="520px">
        <el-form label-width="90px">
          <el-form-item label="CSV 文件">
            <el-upload
              :auto-upload="false"
              :limit="1"
              :on-change="onImportFileChange"
              :show-file-list="true"
            >
              <el-button>选择文件</el-button>
            </el-upload>
          </el-form-item>
          <el-form-item label="内容">
            <el-input
              v-model="importText"
              type="textarea"
              :rows="8"
              class="mono"
              placeholder="每行：BMC地址,用户名,密码（支持双引号转义）"
            />
          </el-form-item>
        </el-form>
        <template #footer>
          <el-button @click="importVisible = false">取消</el-button>
          <el-button type="primary" :loading="importing" :disabled="!importText.trim()" @click="doImport">
            导入
          </el-button>
        </template>
      </el-dialog>
    </div>
  </AppShell>
</template>
